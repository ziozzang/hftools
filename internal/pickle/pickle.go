// Package pickle statically scans Python pickle streams for the import
// references an attacker uses to achieve code execution, without ever
// unpickling (and therefore without ever executing) them. PyTorch checkpoints
// (.bin/.pt/.ckpt/.pth) embed pickles — either raw or inside a zip container —
// so a poisoned checkpoint can run arbitrary code the moment it is loaded. This
// scanner walks the opcode stream, resolves every GLOBAL / STACK_GLOBAL import,
// and flags the ones known to enable command execution.
package pickle

import (
	"archive/zip"
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Severity ranks a finding.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
)

// Global is a resolved (module, name) import reference.
type Global struct {
	Module string `json:"module"`
	Name   string `json:"name"`
}

func (g Global) String() string { return g.Module + "." + g.Name }

// Finding is a flagged import.
type Finding struct {
	Global
	Severity Severity `json:"severity"`
	Reason   string   `json:"reason"`
}

// Result summarizes a single pickle stream.
type Result struct {
	Globals   []Global  `json:"globals,omitempty"`
	Findings  []Finding `json:"findings,omitempty"`
	HasReduce bool      `json:"has_reduce"`
	Opcodes   int       `json:"opcodes"`
	Truncated bool      `json:"truncated,omitempty"`
}

// Report is the result of scanning a file, which may hold several pickle
// members (a torch zip archive) or a single raw pickle.
type Report struct {
	Path       string    `json:"path"`
	Zip        bool      `json:"zip"`
	Skipped    bool      `json:"skipped,omitempty"`
	SkipReason string    `json:"skip_reason,omitempty"`
	Members    []string  `json:"members,omitempty"`
	Globals    []Global  `json:"globals,omitempty"`
	Findings   []Finding `json:"findings,omitempty"`
	Scanned    int       `json:"scanned_streams"`
}

// Dangerous reports whether the report contains any critical finding.
func (r Report) Dangerous() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityCritical {
			return true
		}
	}
	return false
}

// criticalModules are modules whose every attribute enables side effects.
var criticalModules = map[string]bool{
	"os": true, "posix": true, "nt": true, "subprocess": true, "sys": true,
	"socket": true, "shutil": true, "pty": true, "ctypes": true, "importlib": true,
	"runpy": true, "commands": true, "popen2": true, "pip": true, "venv": true,
	"webbrowser": true, "multiprocessing": true, "asyncio": true, "timeit": true,
}

// criticalNames are specific attributes that enable code execution.
var criticalNames = map[string]bool{
	"eval": true, "exec": true, "execfile": true, "compile": true, "open": true,
	"__import__": true, "getattr": true, "setattr": true, "breakpoint": true,
	"apply": true, "system": true, "popen": true, "call": true, "check_call": true,
	"check_output": true, "run": true, "attrgetter": true, "loads": true,
}

// suspiciousModules are worth surfacing but not necessarily malicious.
var suspiciousModules = map[string]bool{
	"builtins": true, "__builtin__": true, "operator": true, "functools": true,
	"pickle": true, "code": true, "codeop": true, "bdb": true, "pdb": true,
	"torch": true, "requests": true, "urllib": true, "base64": true,
}

func classify(g Global) (Finding, bool) {
	if criticalModules[g.Module] {
		return Finding{Global: g, Severity: SeverityCritical, Reason: "import from " + g.Module}, true
	}
	if (g.Module == "builtins" || g.Module == "__builtin__") && criticalNames[g.Name] {
		return Finding{Global: g, Severity: SeverityCritical, Reason: "builtin " + g.Name}, true
	}
	if g.Module == "operator" && g.Name == "attrgetter" {
		return Finding{Global: g, Severity: SeverityCritical, Reason: "operator.attrgetter chains to arbitrary attributes"}, true
	}
	if suspiciousModules[g.Module] {
		return Finding{Global: g, Severity: SeverityWarning, Reason: "import from " + g.Module}, true
	}
	return Finding{}, false
}

// ScanFile scans a file, transparently handling the torch zip container.
func ScanFile(path string) (Report, error) {
	rep := Report{Path: path}
	f, err := os.Open(path)
	if err != nil {
		return rep, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return rep, err
	}
	magic := make([]byte, 4)
	n, _ := io.ReadFull(f, magic)
	isZip := n >= 4 && magic[0] == 'P' && magic[1] == 'K' && magic[2] == 0x03 && magic[3] == 0x04
	// Gate raw scanning on the pickle PROTO opcode (0x80), which every modern
	// (protocol >= 2) pickle — and every torch checkpoint — begins with. This
	// avoids misreading arbitrary binary files (e.g. GGUF/GGML weights that also
	// use a .bin name) as pickle opcodes and reporting phantom imports.
	if !isZip && !(n >= 1 && magic[0] == 0x80) {
		rep.Skipped = true
		rep.SkipReason = "not a pickle stream (no PROTO opcode or zip header)"
		return rep, nil
	}
	if isZip {
		rep.Zip = true
		zr, err := zip.NewReader(f, st.Size())
		if err != nil {
			return rep, fmt.Errorf("open zip: %w", err)
		}
		for _, ze := range zr.File {
			if !looksLikePickle(ze.Name) {
				continue
			}
			rc, err := ze.Open()
			if err != nil {
				return rep, err
			}
			res, err := Scan(rc)
			rc.Close()
			if err != nil {
				return rep, fmt.Errorf("scan %s: %w", ze.Name, err)
			}
			rep.Members = append(rep.Members, ze.Name)
			rep.merge(res)
		}
		return rep.finish(), nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return rep, err
	}
	res, err := Scan(f)
	if err != nil {
		return rep, err
	}
	rep.merge(res)
	return rep.finish(), nil
}

func looksLikePickle(name string) bool {
	lower := strings.ToLower(name)
	base := lower
	if i := strings.LastIndexByte(lower, '/'); i >= 0 {
		base = lower[i+1:]
	}
	return strings.HasSuffix(base, ".pkl") || strings.HasSuffix(base, ".pickle") || base == "data.pkl" || base == "data"
}

func (r *Report) merge(res Result) {
	r.Scanned++
	r.Globals = append(r.Globals, res.Globals...)
	r.Findings = append(r.Findings, res.Findings...)
}

func (r Report) finish() Report {
	r.Globals = dedupeGlobals(r.Globals)
	r.Findings = dedupeFindings(r.Findings)
	sort.Slice(r.Findings, func(i, j int) bool {
		if r.Findings[i].Severity != r.Findings[j].Severity {
			return r.Findings[i].Severity == SeverityCritical
		}
		return r.Findings[i].String() < r.Findings[j].String()
	})
	return r
}

func dedupeGlobals(in []Global) []Global {
	seen := map[Global]bool{}
	var out []Global
	for _, g := range in {
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func dedupeFindings(in []Finding) []Finding {
	seen := map[Global]bool{}
	var out []Finding
	for _, f := range in {
		if !seen[f.Global] {
			seen[f.Global] = true
			out = append(out, f)
		}
	}
	return out
}

const maxOpcodes = 50_000_000
const maxInlineString = 1 << 16

// Scan walks a pickle opcode stream, extracting import references and flagging
// the dangerous ones. It never materializes tensor payloads: length-prefixed
// binary blobs are skipped, not read.
func Scan(r io.Reader) (Result, error) {
	br := bufio.NewReaderSize(r, 1<<16)
	var res Result
	var strings0 []string // recently pushed string values, for STACK_GLOBAL
	pushStr := func(s string) {
		strings0 = append(strings0, s)
		if len(strings0) > 8 {
			strings0 = strings0[len(strings0)-8:]
		}
	}
	record := func(g Global) {
		res.Globals = append(res.Globals, g)
		if f, ok := classify(g); ok {
			res.Findings = append(res.Findings, f)
		}
	}
	for {
		op, err := br.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, err
		}
		res.Opcodes++
		if res.Opcodes > maxOpcodes {
			res.Truncated = true
			break
		}
		switch op {
		case '.': // STOP
			return res.done(), nil
		case 'c': // GLOBAL: module\n name\n
			module, e1 := readLine(br)
			name, e2 := readLine(br)
			if e1 != nil || e2 != nil {
				res.Truncated = true
				return res.done(), nil
			}
			record(Global{Module: module, Name: name})
		case 'i': // INST: module\n name\n
			module, e1 := readLine(br)
			name, e2 := readLine(br)
			if e1 != nil || e2 != nil {
				res.Truncated = true
				return res.done(), nil
			}
			record(Global{Module: module, Name: name})
		case 0x93: // STACK_GLOBAL: module and name from the stack
			if len(strings0) >= 2 {
				record(Global{Module: strings0[len(strings0)-2], Name: strings0[len(strings0)-1]})
			}
		case 'R', 0x91: // REDUCE / ADDITEMS(no) -> only REDUCE matters
			if op == 'R' {
				res.HasReduce = true
			}
		// --- string-producing opcodes (tracked for STACK_GLOBAL) ---
		case 0x8c: // SHORT_BINUNICODE (1-byte len)
			s, err := readSized(br, 1)
			if err != nil {
				res.Truncated = true
				return res.done(), nil
			}
			pushStr(s)
		case 'X': // BINUNICODE (4-byte len)
			s, err := readSized(br, 4)
			if err != nil {
				res.Truncated = true
				return res.done(), nil
			}
			pushStr(s)
		case 0x8d: // BINUNICODE8 (8-byte len)
			s, err := readSized(br, 8)
			if err != nil {
				res.Truncated = true
				return res.done(), nil
			}
			pushStr(s)
		case 'U': // SHORT_BINSTRING (1-byte len)
			s, err := readSized(br, 1)
			if err != nil {
				res.Truncated = true
				return res.done(), nil
			}
			pushStr(s)
		case 'T': // BINSTRING (4-byte len)
			s, err := readSized(br, 4)
			if err != nil {
				res.Truncated = true
				return res.done(), nil
			}
			pushStr(s)
		case 'S', 'V': // STRING / UNICODE (line)
			s, err := readLine(br)
			if err != nil {
				res.Truncated = true
				return res.done(), nil
			}
			pushStr(strings.Trim(s, "'\""))
		// --- length-prefixed binary blobs: skip payload, do not push ---
		case 'B', 'C': // BINBYTES(4) / SHORT_BINBYTES(1)
			sz := 4
			if op == 'C' {
				sz = 1
			}
			if err := skipSized(br, sz); err != nil {
				res.Truncated = true
				return res.done(), nil
			}
		case 0x8e, 0x96: // BINBYTES8 / BYTEARRAY8 (8-byte len)
			if err := skipSized(br, 8); err != nil {
				res.Truncated = true
				return res.done(), nil
			}
		// --- fixed-width inline args (skip N bytes) ---
		case 'J', 0x6a, 0x72, 0x84: // BININT/LONG_BINGET/LONG_BINPUT/EXT4 (4 bytes)
			if err := skip(br, 4); err != nil {
				res.Truncated = true
				return res.done(), nil
			}
		case 'K', 'h', 'q', 0x80, 0x82, 0x8a: // BININT1/BINGET/BINPUT/PROTO/EXT1/LONG1 header
			if op == 0x8a { // LONG1: 1-byte len then that many bytes
				if err := skipSized(br, 1); err != nil {
					res.Truncated = true
					return res.done(), nil
				}
			} else if err := skip(br, 1); err != nil {
				res.Truncated = true
				return res.done(), nil
			}
		case 'M', 0x83: // BININT2 / EXT2 (2 bytes)
			if err := skip(br, 2); err != nil {
				res.Truncated = true
				return res.done(), nil
			}
		case 'G': // BINFLOAT (8 bytes)
			if err := skip(br, 8); err != nil {
				res.Truncated = true
				return res.done(), nil
			}
		case 0x95: // FRAME (8-byte length header)
			if err := skip(br, 8); err != nil {
				res.Truncated = true
				return res.done(), nil
			}
		case 0x8b: // LONG4 (4-byte len + data)
			if err := skipSized(br, 4); err != nil {
				res.Truncated = true
				return res.done(), nil
			}
		// --- line-argument scalars we do not need the value of ---
		case 'F', 'I', 'L', 'P', 'g', 'p': // FLOAT/INT/LONG/PERSID/GET/PUT
			if _, err := readLine(br); err != nil {
				res.Truncated = true
				return res.done(), nil
			}
		default:
			// All remaining opcodes are zero-argument stack ops
			// (MARK, POP, TUPLE*, DICT, LIST, MEMOIZE, NEWOBJ, BUILD, ...).
			// They do not desync the stream, so nothing to consume.
		}
	}
	return res.done(), nil
}

func (r Result) done() Result {
	r.Globals = dedupeGlobals(r.Globals)
	return r
}

func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) > maxInlineString {
		line = line[:maxInlineString]
	}
	return line, nil
}

func readUint(br *bufio.Reader, n int) (uint64, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return 0, err
	}
	var v uint64
	for i := 0; i < n; i++ {
		v |= uint64(buf[i]) << (8 * i) // little-endian
	}
	return v, nil
}

// readSized reads a lenBytes-wide little-endian length then that many bytes,
// returning them as a string but materializing at most maxInlineString bytes
// (module/name references are short; anything larger is skipped).
func readSized(br *bufio.Reader, lenBytes int) (string, error) {
	n, err := readUint(br, lenBytes)
	if err != nil {
		return "", err
	}
	if n > maxInlineString {
		if _, err := io.CopyN(io.Discard, br, int64(n)); err != nil {
			return "", err
		}
		return "", nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func skipSized(br *bufio.Reader, lenBytes int) error {
	n, err := readUint(br, lenBytes)
	if err != nil {
		return err
	}
	_, err = io.CopyN(io.Discard, br, int64(n))
	return err
}

func skip(br *bufio.Reader, n int) error {
	_, err := io.CopyN(io.Discard, br, int64(n))
	return err
}
