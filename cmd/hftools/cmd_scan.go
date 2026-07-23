package main

import (
	"context"
	"errors"
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
func scanCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	quiet := fs.Bool("quiet", false, "only report files with findings")
	all := fs.Bool("all", false, "scan every file argument, ignoring the extension filter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := watchInterrupt(ctx)
	defer stop()
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

	reports, dangerous, warnings, err := scanTargets(ctx, targets)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errInterrupted
		}
		return err
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

// scanTargets statically scans each file, returning the reports and the running
// count of critical and warning findings. It stops early with the context error
// when the user interrupts (Ctrl+C / ESC).
func scanTargets(ctx context.Context, targets []string) (reports []pickle.Report, dangerous, warnings int, err error) {
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return reports, dangerous, warnings, err
		}
		rep, err := pickle.ScanFile(t)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("scan %s: %w", t, err)
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
	return reports, dangerous, warnings, nil
}

// scanRepositoryDir walks a repository directory for pickle/torch checkpoints and
// scans each for imports that enable code execution on load. It is the hook
// behind `verify`/`verify-batch --scan`.
func scanRepositoryDir(ctx context.Context, root string) (reports []pickle.Report, dangerous, warnings int, err error) {
	var targets []string
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if err := ctx.Err(); err != nil {
			return err
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
	if walkErr != nil {
		return nil, 0, 0, walkErr
	}
	sort.Strings(targets)
	return scanTargets(ctx, targets)
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
