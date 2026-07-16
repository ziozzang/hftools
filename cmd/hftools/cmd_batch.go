package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ziozzang/hfdownload/internal/hub"
)

func batchCommand(ctx context.Context, args []string) error {
	cfg, configPath, err := loadSettings(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("batch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var queuePath, listPath, outputRoot, defaultRepoType string
	var continueOnError bool
	addTransferFlags(fs, &cfg, &configPath)
	fs.StringVar(&queuePath, "queue", "", "JSON queue or line-based repository list")
	fs.StringVar(&listPath, "list", "", "line-based repository list (';' comments and blank lines allowed)")
	fs.StringVar(&outputRoot, "output-root", "", "root for automatically named repository directories")
	fs.StringVar(&defaultRepoType, "type", "model", "repository type for list entries: model or dataset")
	fs.BoolVar(&continueOnError, "continue-on-error", false, "continue with remaining jobs after a failure")
	if err := fs.Parse(args); err != nil {
		return err
	}
	applyTag(&cfg)
	if err := hub.RepoType(defaultRepoType).Validate(); err != nil {
		return err
	}
	if queuePath != "" && listPath != "" {
		return fmt.Errorf("use only one of --queue and --list")
	}
	if listPath != "" {
		queuePath = listPath
	}
	if queuePath == "" {
		if fs.NArg() == 1 {
			queuePath = fs.Arg(0)
		} else {
			return fmt.Errorf("usage: hftools batch [options] --queue FILE")
		}
	} else if fs.NArg() != 0 {
		return fmt.Errorf("queue supplied both with --queue and as an argument")
	}
	b, err := os.ReadFile(queuePath)
	if err != nil {
		return err
	}
	q, err := parseQueueData(b)
	if err != nil {
		return err
	}
	if outputRoot != "" {
		q.OutputRoot = outputRoot
	}
	if len(q.Jobs) == 0 {
		return fmt.Errorf("queue contains no jobs")
	}
	fmt.Fprintf(os.Stderr, "batch plan: %d repositories\n", len(q.Jobs))
	var failed []string
	for i, job := range q.Jobs {
		if job.Repo == "" {
			return fmt.Errorf("queue job %d has no repo", i+1)
		}
		normalizedRepo, err := hub.NormalizeRepoID(job.Repo)
		if err != nil {
			return fmt.Errorf("queue job %d: %w", i+1, err)
		}
		jobCfg := cfg
		jobRepoType := hub.RepoType(defaultRepoType)
		if job.RepoType != "" {
			jobRepoType = hub.RepoType(job.RepoType)
			if err := jobRepoType.Validate(); err != nil {
				return fmt.Errorf("queue job %d: %w", i+1, err)
			}
		}
		if job.Revision != "" {
			jobCfg.Revision = job.Revision
		}
		if job.Parts != nil {
			jobCfg.Parts = *job.Parts
		}
		if job.MultipartThreshold != nil {
			jobCfg.MultipartThreshold = *job.MultipartThreshold
		}
		if job.BufferSize != nil {
			jobCfg.BufferSize = *job.BufferSize
		}
		if job.Retries != nil {
			jobCfg.Retries = *job.Retries
		}
		if job.Resume != nil {
			jobCfg.Resume = *job.Resume
		}
		if job.Filters != nil {
			jobCfg.Filters = append([]string(nil), job.Filters...)
		}
		if job.Output != "" {
			jobCfg.Output = job.Output
		} else {
			outputRoot := q.OutputRoot
			if outputRoot == "" {
				outputRoot = "."
			}
			jobCfg.Output = filepath.Join(outputRoot, hub.LocalDirectoryName(normalizedRepo))
		}
		fmt.Fprintf(os.Stderr, "\n[%d/%d] %s %s -> %s\n", i+1, len(q.Jobs), jobRepoType, normalizedRepo, jobCfg.Output)
		if err := syncRepository(ctx, jobCfg, normalizedRepo, jobRepoType); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", job.Repo, err))
			if !continueOnError {
				return errors.New(failed[0])
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d queue job(s) failed:\n  %s", len(failed), strings.Join(failed, "\n  "))
	}
	fmt.Fprintf(os.Stderr, "batch complete: %d/%d repositories\n", len(q.Jobs), len(q.Jobs))
	return nil
}

func parseQueueData(data []byte) (queueFile, error) {
	content := strings.TrimPrefix(string(data), "\ufeff")
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "{") {
		var q queueFile
		if err := json.Unmarshal([]byte(trimmed), &q); err != nil {
			return queueFile{}, fmt.Errorf("JSON queue parse: %w", err)
		}
		return q, nil
	}

	var q queueFile
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		repoID, err := hub.NormalizeRepoID(line)
		if err != nil {
			return queueFile{}, fmt.Errorf("repository list line %d: %w", lineNumber, err)
		}
		q.Jobs = append(q.Jobs, queueJob{Repo: repoID})
	}
	if err := scanner.Err(); err != nil {
		return queueFile{}, fmt.Errorf("repository list read: %w", err)
	}
	return q, nil
}
