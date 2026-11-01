package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed skills/veems/SKILL.md
var skillFS embed.FS

// skillDirs lists the user-level skill folders agents read. ~/.agents/skills
// is the shared one (Codex, OpenCode, oh-my-pi, pi); the others cover agents
// that only look in their own folder.
func skillDirs() []string {
	h, _ := os.UserHomeDir()
	dirs := []string{filepath.Join(h, ".agents", "skills")}
	for _, d := range []struct{ agent, skills string }{
		{".claude", ".claude/skills"},
		{".codex", ".codex/skills"},
		{".pi", ".pi/agent/skills"},
	} {
		if exists(filepath.Join(h, d.agent)) {
			dirs = append(dirs, filepath.Join(h, d.skills))
		}
	}
	return dirs
}

func cmdSkill(args []string) error {
	body, _ := skillFS.ReadFile("skills/veems/SKILL.md")
	if len(args) == 0 || args[0] == "show" {
		os.Stdout.Write(body)
		return nil
	}
	dirs := skillDirs()
	if len(args) == 3 && args[1] == "--dir" {
		dirs = []string{args[2]}
	}
	switch args[0] {
	case "install":
		for _, d := range dirs {
			p := filepath.Join(d, "veems", "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, body, 0o644); err != nil {
				return err
			}
			fmt.Println("installed", p)
		}
	case "uninstall":
		for _, d := range dirs {
			os.RemoveAll(filepath.Join(d, "veems"))
			fmt.Println("removed", filepath.Join(d, "veems"))
		}
	default:
		return fmt.Errorf("usage: veems skill [show|install|uninstall] [--dir DIR]")
	}
	return nil
}
