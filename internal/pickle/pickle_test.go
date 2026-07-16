package pickle

import (
	"bytes"
	"testing"
)

type pb struct{ b bytes.Buffer }

func (p *pb) op(o byte) { p.b.WriteByte(o) }
func (p *pb) shortStr(s string) {
	p.b.WriteByte(0x8c) // SHORT_BINUNICODE
	p.b.WriteByte(byte(len(s)))
	p.b.WriteString(s)
	p.b.WriteByte(0x94) // MEMOIZE
}
func (p *pb) globalText(module, name string) {
	p.b.WriteByte('c')
	p.b.WriteString(module)
	p.b.WriteByte('\n')
	p.b.WriteString(name)
	p.b.WriteByte('\n')
}

func hasFinding(res Result, module, name string, sev Severity) bool {
	for _, f := range res.Findings {
		if f.Module == module && f.Name == name && f.Severity == sev {
			return true
		}
	}
	return false
}

func TestScanStackGlobalCritical(t *testing.T) {
	p := &pb{}
	p.op(0x80)
	p.op(0x04) // PROTO 4
	p.shortStr("os")
	p.shortStr("system")
	p.op(0x93) // STACK_GLOBAL -> os.system
	p.shortStr("echo pwned")
	p.op(0x85) // TUPLE1
	p.op('R')  // REDUCE
	p.op('.')  // STOP

	res, err := Scan(&p.b)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !res.HasReduce {
		t.Fatalf("expected REDUCE detected")
	}
	if !hasFinding(res, "os", "system", SeverityCritical) {
		t.Fatalf("expected critical os.system finding, got %+v", res.Findings)
	}
}

func TestScanBenignGlobal(t *testing.T) {
	p := &pb{}
	p.globalText("collections", "OrderedDict")
	p.op('.')
	res, err := Scan(&p.b)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected no findings, got %+v", res.Findings)
	}
	found := false
	for _, g := range res.Globals {
		if g.Module == "collections" && g.Name == "OrderedDict" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected collections.OrderedDict listed in globals")
	}
}

func TestScanBuiltinEvalCritical(t *testing.T) {
	p := &pb{}
	p.globalText("builtins", "eval")
	p.op('.')
	res, _ := Scan(&p.b)
	if !hasFinding(res, "builtins", "eval", SeverityCritical) {
		t.Fatalf("expected critical builtins.eval, got %+v", res.Findings)
	}
}

// A BINBYTES payload between opcodes must be skipped without desyncing the
// stream, so a dangerous global that follows it is still detected.
func TestScanSkipsBinaryPayload(t *testing.T) {
	p := &pb{}
	p.op('C') // SHORT_BINBYTES, 1-byte len
	p.b.WriteByte(5)
	p.b.Write([]byte{0x63, 0x63, 0x63, 0x63, 0x63}) // 5 bytes of 'c' as payload, must NOT be read as GLOBAL
	p.globalText("subprocess", "Popen")
	p.op('.')
	res, _ := Scan(&p.b)
	if !hasFinding(res, "subprocess", "Popen", SeverityCritical) {
		t.Fatalf("expected subprocess.Popen after binary payload, got %+v", res.Findings)
	}
	for _, g := range res.Globals {
		if g.Module == "cccc" || g.Module == "ccc" {
			t.Fatalf("payload bytes were misparsed as a global: %+v", res.Globals)
		}
	}
}
