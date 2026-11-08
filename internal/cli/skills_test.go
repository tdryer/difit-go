package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/tdryer/difit-go/internal/comments"
)

// The agent skills in skills/ tell agents which commands to run; check that
// those commands still exist and that the example comments parse.
func TestSkillsMatchCLI(t *testing.T) {
	files, err := filepath.Glob("../../skills/*/SKILL.md")
	if err != nil || len(files) == 0 {
		t.Fatalf("no skills found: %v", err)
	}
	root := (&app{}).rootCommand()
	flags := map[string]bool{"help": true}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		cmd.Flags().VisitAll(func(f *pflag.Flag) { flags[f.Name] = true })
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(root)

	flagPattern := regexp.MustCompile(`(?:^|[\s(` + "`" + `])--([a-z][a-z-]*)`)
	subcommandPattern := regexp.MustCompile(`difit comment ([a-z]+)`)
	commentPattern := regexp.MustCompile(`--comment '([^']*)'`)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, m := range flagPattern.FindAllStringSubmatch(text, -1) {
			if !flags[m[1]] {
				t.Errorf("%s: unknown flag --%s", file, m[1])
			}
		}
		for _, m := range subcommandPattern.FindAllStringSubmatch(text, -1) {
			if cmd, _, err := root.Find([]string{"comment", m[1]}); err != nil || cmd.Name() != m[1] {
				t.Errorf("%s: unknown command difit comment %s", file, m[1])
			}
		}
		examples := commentPattern.FindAllStringSubmatch(text, -1)
		for _, m := range examples {
			if _, err := comments.ParseImportValue(m[1]); err != nil {
				t.Errorf("%s: example comment %s: %v", file, m[1], err)
			}
		}
		if len(examples) == 0 {
			t.Errorf("%s: no --comment examples found", file)
		}
	}
}
