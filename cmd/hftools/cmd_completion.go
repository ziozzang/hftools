package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// completionCommands is the list of top-level subcommands offered by shell
// completion. Keep it aligned with run()'s dispatch.
var completionCommands = []string{
	"download", "dataset", "space", "batch", "verify", "verify-batch", "status",
	"info", "ls", "refs", "search", "whoami", "diff", "du", "get", "peek",
	"scan", "sign", "verify-sig",
	"gc", "cache-gc", "dedup", "repair", "doctor", "watch",
	"cache-export", "cache-import", "cache-import-batch", "cache-list", "cache-scan", "cache-verify",
	"serve", "completion", "update", "version", "help",
}

func completionCommand(args []string) error {
	fs := flag.NewFlagSet("completion", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: hftools completion [bash|zsh|fish]")
	}
	cmds := strings.Join(completionCommands, " ")
	switch fs.Arg(0) {
	case "bash":
		fmt.Printf(bashCompletion, cmds)
	case "zsh":
		fmt.Printf(zshCompletion, cmds)
	case "fish":
		fmt.Print(fishCompletion())
	default:
		return fmt.Errorf("unsupported shell %q (want bash, zsh, or fish)", fs.Arg(0))
	}
	return nil
}

const bashCompletion = `# hftools bash completion
# install: hftools completion bash > /etc/bash_completion.d/hftools
_hftools() {
    local commands="%s"
    if [ "$COMP_CWORD" -eq 1 ]; then
        COMPREPLY=( $(compgen -W "$commands" -- "${COMP_WORDS[1]}") )
    else
        COMPREPLY=( $(compgen -f -- "${COMP_WORDS[COMP_CWORD]}") )
    fi
}
complete -F _hftools hftools
`

const zshCompletion = `#compdef hftools
# install: hftools completion zsh > "${fpath[1]}/_hftools"
_hftools() {
    local -a commands
    commands=(%s)
    if (( CURRENT == 2 )); then
        compadd -- $commands
    else
        _files
    fi
}
compdef _hftools hftools
`

func fishCompletion() string {
	var b strings.Builder
	b.WriteString("# hftools fish completion\n")
	b.WriteString("# install: hftools completion fish > ~/.config/fish/completions/hftools.fish\n")
	b.WriteString("complete -c hftools -f\n")
	for _, c := range completionCommands {
		fmt.Fprintf(&b, "complete -c hftools -n __fish_use_subcommand -a %s\n", c)
	}
	return b.String()
}
