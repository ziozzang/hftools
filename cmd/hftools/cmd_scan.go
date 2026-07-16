package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ziozzang/hftools/internal/pickle"
)

var pickleExtensions = []string{".bin", ".pt", ".pth", ".ckpt", ".pkl", ".pickle"}

func isPickleCandidate(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range pickleExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// scanCommand statically inspects PyTorch/pickle checkpoints for the import
// references that enable code execution on load.
func scanCommand(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	quiet := fs.Bool("quiet", false, "only report files with findings")
	all := fs.Bool("all", false, "scan every file argument, ignoring the extension filter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths := fs.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}
	var targets []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		if st.IsDir() {
			err = filepath.WalkDir(p, func(path string, d os.DirEntry, werr error) error {
				if werr != nil {
					return werr
				}
				if d.IsDir() {
					if d.Name() == ".metadata" || d.Name() == "tmp" {
						return filepath.SkipDir
					}
					return nil
				}
				if isPickleCandidate(d.Name()) {
					targets = append(targets, path)
				}
				return nil
			})
			if err != nil {
				return err
			}
		} else if *all || isPickleCandidate(st.Name()) {
			targets = append(targets, p)
		} else {
			// An explicit non-candidate file: scan it anyway (user asked directly).
			targets = append(targets, p)
		}
	}
	sort.Strings(targets)

	var reports []pickle.Report
	dangerous := 0
	warnings := 0
	for _, t := range targets {
		rep, err := pickle.ScanFile(t)
		if err != nil {
			return fmt.Errorf("scan %s: %w", t, err)
		}
		reports = append(reports, rep)
		for _, f := range rep.Findings {
			if f.Severity == pickle.SeverityCritical {
				dangerous++
			} else {
				warnings++
			}
		}
	}

	if *jsonOut {
		if err := printJSON(os.Stdout, reports); err != nil {
			return err
		}
	} else {
		for _, rep := range reports {
			printScanReport(rep, *quiet)
		}
		fmt.Printf("scanned %d file(s): %d critical, %d warning(s)\n", len(reports), dangerous, warnings)
	}
	if dangerous > 0 {
		return fmt.Errorf("%d dangerous import(s) found; do not load these files", dangerous)
	}
	return nil
}

func printScanReport(rep pickle.Report, quiet bool) {
	if rep.Skipped {
		if !quiet {
			fmt.Printf("skip  %s (%s)\n", rep.Path, rep.SkipReason)
		}
		return
	}
	if len(rep.Findings) == 0 {
		if !quiet {
			fmt.Printf("ok    %s (%d import(s))\n", rep.Path, len(rep.Globals))
		}
		return
	}
	status := "WARN "
	if rep.Dangerous() {
		status = "DANGER"
	}
	fmt.Printf("%s %s\n", status, rep.Path)
	if rep.Zip && len(rep.Members) > 0 {
		fmt.Printf("      members: %s\n", strings.Join(rep.Members, ", "))
	}
	for _, f := range rep.Findings {
		fmt.Printf("      [%s] %s.%s — %s\n", f.Severity, f.Module, f.Name, f.Reason)
	}
}
