// Package modelfile parses the on-disk headers of the two model container
// formats used across the Hugging Face Hub — safetensors and GGUF — without
// reading tensor data. The parsers operate on a byte window (typically fetched
// with an HTTP Range request) so a multi-gigabyte checkpoint can be inspected
// by transferring only its header.
package modelfile

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
)

// Format identifies a recognized container.
type Format string

const (
	FormatSafetensors Format = "safetensors"
	FormatGGUF        Format = "gguf"
	FormatUnknown     Format = ""
)

// Tensor is one named tensor entry.
type Tensor struct {
	Name  string  `json:"name"`
	DType string  `json:"dtype"`
	Shape []int64 `json:"shape"`
}

// Params returns the element count implied by the shape.
func (t Tensor) Params() int64 {
	n := int64(1)
	for _, d := range t.Shape {
		if d <= 0 {
			return 0
		}
		n *= d
	}
	if len(t.Shape) == 0 {
		return 0
	}
	return n
}

// DTypeStat aggregates tensors sharing a dtype.
type DTypeStat struct {
	DType  string `json:"dtype"`
	Count  int    `json:"count"`
	Params int64  `json:"params"`
}

// Info is the parsed header summary.
type Info struct {
	Format      Format            `json:"format"`
	Tensors     int               `json:"tensors"`
	Params      int64             `json:"params"`
	ParamsKnown bool              `json:"params_known"`
	DataBytes   int64             `json:"data_bytes,omitempty"`
	HeaderBytes int64             `json:"header_bytes,omitempty"`
	Version     int               `json:"version,omitempty"`
	Arch        string            `json:"arch,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	DTypes      []DTypeStat       `json:"dtypes,omitempty"`
	TensorList  []Tensor          `json:"tensor_list,omitempty"`
	// Partial reports that parsing stopped before the end of the header because
	// the supplied window was too small (GGUF metadata can be large).
	Partial bool `json:"partial,omitempty"`
}

// Detect guesses the container format from a leading window.
func Detect(head []byte) Format {
	if len(head) >= 4 && string(head[:4]) == "GGUF" {
		return FormatGGUF
	}
	if len(head) >= 9 {
		n := binary.LittleEndian.Uint64(head[:8])
		// A safetensors header is a JSON object, so byte 8 is '{' (optionally
		// preceded by whitespace) and the declared length is sane.
		if n >= 2 && n < (1<<34) {
			for _, c := range head[8:] {
				if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
					continue
				}
				if c == '{' {
					return FormatSafetensors
				}
				break
			}
		}
	}
	return FormatUnknown
}

// SafetensorsHeaderLen decodes the little-endian header length from the first
// eight bytes of a safetensors file.
func SafetensorsHeaderLen(first8 []byte) (int64, error) {
	if len(first8) < 8 {
		return 0, io.ErrUnexpectedEOF
	}
	n := binary.LittleEndian.Uint64(first8[:8])
	if n == 0 || n > (1<<34) {
		return 0, fmt.Errorf("implausible safetensors header length %d", n)
	}
	return int64(n), nil
}

type stHeaderEntry struct {
	DType       string  `json:"dtype"`
	Shape       []int64 `json:"shape"`
	DataOffsets []int64 `json:"data_offsets"`
}

// ParseSafetensors parses the JSON header (the N bytes that follow the 8-byte
// length prefix). headerLen is that prefix value, used only to report
// HeaderBytes.
func ParseSafetensors(headerJSON []byte, headerLen int64) (*Info, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(headerJSON, &raw); err != nil {
		return nil, fmt.Errorf("safetensors header decode: %w", err)
	}
	info := &Info{Format: FormatSafetensors, HeaderBytes: 8 + headerLen, ParamsKnown: true, Metadata: map[string]string{}}
	byType := map[string]*DTypeStat{}
	var maxOffset int64
	for name, rm := range raw {
		if name == "__metadata__" {
			var md map[string]string
			if json.Unmarshal(rm, &md) == nil {
				for k, v := range md {
					info.Metadata[k] = v
				}
			}
			continue
		}
		var e stHeaderEntry
		if err := json.Unmarshal(rm, &e); err != nil {
			return nil, fmt.Errorf("safetensors tensor %q: %w", name, err)
		}
		t := Tensor{Name: name, DType: e.DType, Shape: e.Shape}
		info.Tensors++
		p := t.Params()
		info.Params += p
		if len(e.DataOffsets) == 2 && e.DataOffsets[1] > maxOffset {
			maxOffset = e.DataOffsets[1]
		}
		st := byType[e.DType]
		if st == nil {
			st = &DTypeStat{DType: e.DType}
			byType[e.DType] = st
		}
		st.Count++
		st.Params += p
		info.TensorList = append(info.TensorList, t)
	}
	info.DataBytes = maxOffset
	sort.Slice(info.TensorList, func(i, j int) bool { return info.TensorList[i].Name < info.TensorList[j].Name })
	for _, st := range byType {
		info.DTypes = append(info.DTypes, *st)
	}
	sort.Slice(info.DTypes, func(i, j int) bool { return info.DTypes[i].Params > info.DTypes[j].Params })
	if len(info.Metadata) == 0 {
		info.Metadata = nil
	}
	return info, nil
}

// GGUF value type tags (gguf spec).
const (
	ggufUint8 uint32 = iota
	ggufInt8
	ggufUint16
	ggufInt16
	ggufUint32
	ggufInt32
	ggufFloat32
	ggufBool
	ggufString
	ggufArray
	ggufUint64
	ggufInt64
	ggufFloat64
)

// ParseGGUF parses a GGUF header from window. GGUF places its (potentially
// large) metadata before the tensor descriptors, so a window that is too small
// yields a Partial result: the version, tensor count, and whatever metadata
// fit are still returned.
func ParseGGUF(window []byte) (*Info, error) {
	c := &cursor{b: window}
	magic, err := c.bytes(4)
	if err != nil || string(magic) != "GGUF" {
		return nil, fmt.Errorf("not a GGUF file")
	}
	info := &Info{Format: FormatGGUF, ParamsKnown: false, Metadata: map[string]string{}}
	version, err := c.u32()
	if err != nil {
		return nil, err
	}
	info.Version = int(version)
	tensorCount, err := c.u64()
	if err != nil {
		return nil, err
	}
	info.Tensors = int(tensorCount)
	kvCount, err := c.u64()
	if err != nil {
		return nil, err
	}
	for i := uint64(0); i < kvCount; i++ {
		key, err := c.gstring()
		if err != nil {
			info.Partial = true
			finishGGUF(info)
			return info, nil
		}
		val, err := c.gvalue()
		if err != nil {
			info.Partial = true
			finishGGUF(info)
			return info, nil
		}
		if val != "" {
			info.Metadata[key] = val
		}
		if key == "general.architecture" {
			info.Arch = val
		}
	}
	// Metadata fully parsed; try tensor descriptors to compute params.
	total := int64(0)
	ok := true
	for i := uint64(0); i < tensorCount; i++ {
		if _, err := c.gstring(); err != nil {
			ok = false
			break
		}
		nDims, err := c.u32()
		if err != nil {
			ok = false
			break
		}
		p := int64(1)
		for d := uint32(0); d < nDims; d++ {
			dim, err := c.u64()
			if err != nil {
				ok = false
				break
			}
			p *= int64(dim)
		}
		if !ok {
			break
		}
		if _, err := c.u32(); err != nil { // ggml type
			ok = false
			break
		}
		if _, err := c.u64(); err != nil { // offset
			ok = false
			break
		}
		total += p
	}
	if ok {
		info.Params = total
		info.ParamsKnown = true
	} else {
		info.Partial = true
	}
	finishGGUF(info)
	return info, nil
}

func finishGGUF(info *Info) {
	if len(info.Metadata) == 0 {
		info.Metadata = nil
	}
}

type cursor struct {
	b []byte
	i int
}

func (c *cursor) bytes(n int) ([]byte, error) {
	if n < 0 || c.i+n > len(c.b) {
		return nil, io.ErrUnexpectedEOF
	}
	out := c.b[c.i : c.i+n]
	c.i += n
	return out, nil
}

func (c *cursor) u8() (uint8, error) {
	b, err := c.bytes(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (c *cursor) u16() (uint16, error) {
	b, err := c.bytes(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

func (c *cursor) u32() (uint32, error) {
	b, err := c.bytes(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (c *cursor) u64() (uint64, error) {
	b, err := c.bytes(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

// gstring reads a GGUF string (uint64 length + raw bytes), guarding the length
// against the remaining window so a truncated buffer fails cleanly.
func (c *cursor) gstring() (string, error) {
	n, err := c.u64()
	if err != nil {
		return "", err
	}
	if n > uint64(len(c.b)-c.i) {
		return "", io.ErrUnexpectedEOF
	}
	b, err := c.bytes(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// gvalue reads a typed metadata value and renders it as a short string. Arrays
// are summarized (element type and length) rather than expanded.
func (c *cursor) gvalue() (string, error) {
	t, err := c.u32()
	if err != nil {
		return "", err
	}
	return c.gvalueOf(t)
}

func (c *cursor) gvalueOf(t uint32) (string, error) {
	switch t {
	case ggufUint8:
		v, err := c.u8()
		return strconv.FormatUint(uint64(v), 10), err
	case ggufInt8:
		v, err := c.u8()
		return strconv.FormatInt(int64(int8(v)), 10), err
	case ggufUint16:
		v, err := c.u16()
		return strconv.FormatUint(uint64(v), 10), err
	case ggufInt16:
		v, err := c.u16()
		return strconv.FormatInt(int64(int16(v)), 10), err
	case ggufUint32:
		v, err := c.u32()
		return strconv.FormatUint(uint64(v), 10), err
	case ggufInt32:
		v, err := c.u32()
		return strconv.FormatInt(int64(int32(v)), 10), err
	case ggufFloat32:
		v, err := c.u32()
		if err != nil {
			return "", err
		}
		return strconv.FormatFloat(float64(math.Float32frombits(v)), 'g', -1, 32), nil
	case ggufBool:
		v, err := c.u8()
		if err != nil {
			return "", err
		}
		if v != 0 {
			return "true", nil
		}
		return "false", nil
	case ggufString:
		return c.gstring()
	case ggufUint64:
		v, err := c.u64()
		return strconv.FormatUint(v, 10), err
	case ggufInt64:
		v, err := c.u64()
		return strconv.FormatInt(int64(v), 10), err
	case ggufFloat64:
		v, err := c.u64()
		if err != nil {
			return "", err
		}
		return strconv.FormatFloat(math.Float64frombits(v), 'g', -1, 64), nil
	case ggufArray:
		elemType, err := c.u32()
		if err != nil {
			return "", err
		}
		n, err := c.u64()
		if err != nil {
			return "", err
		}
		for i := uint64(0); i < n; i++ {
			if _, err := c.gvalueOf(elemType); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("[%s x %d]", ggufTypeName(elemType), n), nil
	default:
		return "", fmt.Errorf("unknown gguf value type %d", t)
	}
}

func ggufTypeName(t uint32) string {
	switch t {
	case ggufUint8:
		return "u8"
	case ggufInt8:
		return "i8"
	case ggufUint16:
		return "u16"
	case ggufInt16:
		return "i16"
	case ggufUint32:
		return "u32"
	case ggufInt32:
		return "i32"
	case ggufFloat32:
		return "f32"
	case ggufBool:
		return "bool"
	case ggufString:
		return "str"
	case ggufUint64:
		return "u64"
	case ggufInt64:
		return "i64"
	case ggufFloat64:
		return "f64"
	case ggufArray:
		return "array"
	default:
		return "?"
	}
}
