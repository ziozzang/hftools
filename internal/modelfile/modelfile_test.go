package modelfile

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func buildSafetensors(headerJSON string) []byte {
	var buf bytes.Buffer
	length := make([]byte, 8)
	binary.LittleEndian.PutUint64(length, uint64(len(headerJSON)))
	buf.Write(length)
	buf.WriteString(headerJSON)
	return buf.Bytes()
}

func TestParseSafetensors(t *testing.T) {
	header := `{"__metadata__":{"format":"pt"},"a.weight":{"dtype":"F16","shape":[2,3],"data_offsets":[0,12]},"b.weight":{"dtype":"F32","shape":[4],"data_offsets":[12,28]}}`
	raw := buildSafetensors(header)
	if got := Detect(raw); got != FormatSafetensors {
		t.Fatalf("Detect = %q, want safetensors", got)
	}
	n, err := SafetensorsHeaderLen(raw[:8])
	if err != nil {
		t.Fatalf("len: %v", err)
	}
	info, err := ParseSafetensors(raw[8:8+n], n)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.Tensors != 2 {
		t.Fatalf("tensors = %d, want 2", info.Tensors)
	}
	if info.Params != 2*3+4 {
		t.Fatalf("params = %d, want 10", info.Params)
	}
	if info.DataBytes != 28 {
		t.Fatalf("data bytes = %d, want 28", info.DataBytes)
	}
	if info.Metadata["format"] != "pt" {
		t.Fatalf("metadata format = %q", info.Metadata["format"])
	}
	if len(info.DTypes) != 2 {
		t.Fatalf("dtypes = %d, want 2", len(info.DTypes))
	}
}

type ggufBuilder struct{ buf bytes.Buffer }

func (b *ggufBuilder) u32(v uint32) {
	tmp := make([]byte, 4)
	binary.LittleEndian.PutUint32(tmp, v)
	b.buf.Write(tmp)
}
func (b *ggufBuilder) u64(v uint64) {
	tmp := make([]byte, 8)
	binary.LittleEndian.PutUint64(tmp, v)
	b.buf.Write(tmp)
}
func (b *ggufBuilder) str(s string) { b.u64(uint64(len(s))); b.buf.WriteString(s) }

func TestParseGGUF(t *testing.T) {
	b := &ggufBuilder{}
	b.buf.WriteString("GGUF")
	b.u32(3) // version
	b.u64(1) // tensor count
	b.u64(2) // kv count
	// kv 1: general.architecture = "llama" (string type 8)
	b.str("general.architecture")
	b.u32(8)
	b.str("llama")
	// kv 2: some.count = 7 (uint32 type 4)
	b.str("some.count")
	b.u32(4)
	b.u32(7)
	// tensor info: name, n_dims=2, dims=[4,8], type=0, offset=0
	b.str("token_embd.weight")
	b.u32(2)
	b.u64(4)
	b.u64(8)
	b.u32(0)
	b.u64(0)

	raw := b.buf.Bytes()
	if got := Detect(raw); got != FormatGGUF {
		t.Fatalf("Detect = %q, want gguf", got)
	}
	info, err := ParseGGUF(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.Version != 3 {
		t.Fatalf("version = %d, want 3", info.Version)
	}
	if info.Tensors != 1 {
		t.Fatalf("tensors = %d, want 1", info.Tensors)
	}
	if info.Arch != "llama" {
		t.Fatalf("arch = %q, want llama", info.Arch)
	}
	if !info.ParamsKnown || info.Params != 32 {
		t.Fatalf("params = %d known=%v, want 32 true", info.Params, info.ParamsKnown)
	}
	if info.Partial {
		t.Fatalf("unexpected partial")
	}
}

func TestParseGGUFPartial(t *testing.T) {
	b := &ggufBuilder{}
	b.buf.WriteString("GGUF")
	b.u32(3)
	b.u64(5)   // claims 5 tensors
	b.u64(100) // claims 100 kv but provides none
	info, err := ParseGGUF(b.buf.Bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !info.Partial {
		t.Fatalf("expected partial result")
	}
	if info.Tensors != 5 || info.Version != 3 {
		t.Fatalf("header fields not recovered: %+v", info)
	}
}
