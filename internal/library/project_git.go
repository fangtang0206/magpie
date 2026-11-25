package library

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/yetone/magpie/internal/edit"
)

// A project in a git repository keeps what magpie places in it out of git
// through the repository's own info/exclude, which git reads beside the
// .gitignore and which is never committed (gitignore(5)): nothing of
// magpie's goes into the project's history, its .gitignore included. The
// file is shared by the repository's worktrees, each reading it against
// its own top folder, so one block of lines keeps a project's entries out
// of git in every worktree.

// gitWhere is where the project's repository is: Top, the worktree's top
// folder the project is in; Common, the repository's .git folder that
// every worktree shares (info/exclude, worktrees/).
type gitWhere struct {
	Top, Common string
}

// gitOf is the repository dir is in, by the nearest .git above it: a
// folder (the main worktree's), or a file naming the worktree's own
// folder under the main .git (git worktree add), whose commondir names
// the shared one. ok is false outside a repository.
func gitOf(dir string) (gitWhere, bool) {
	for d := dir; ; {
		g := filepath.Join(d, ".git")
		if fi, err := os.Stat(g); err == nil {
			if fi.IsDir() {
				return gitWhere{Top: d, Common: g}, true
			}
			if own, ok := gitdirOf(g); ok {
				common := own
				if b, err := os.ReadFile(filepath.Join(own, "commondir")); err == nil {
					c := strings.TrimSpace(string(b))
					if !filepath.IsAbs(c) {
						c = filepath.Join(own, c)
					}
					common = filepath.Clean(c)
				}
				return gitWhere{Top: d, Common: common}, true
			}
		}
		up := filepath.Dir(d)
		if up == d {
			return gitWhere{}, false
		}
		d = up
	}
}

// gitdirOf is the folder a .git file names ("gitdir: <path>").
func gitdirOf(file string) (string, bool) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	g, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return "", false
	}
	g = strings.TrimSpace(g)
	if !filepath.IsAbs(g) {
		g = filepath.Join(filepath.Dir(file), g)
	}
	return filepath.Clean(g), true
}

// mainTop is the main worktree's top folder: the folder the shared .git is
// in. "" for a bare repository's worktrees, which have no main one.
func (g gitWhere) mainTop() string {
	if filepath.Base(g.Common) != ".git" {
		return ""
	}
	return filepath.Dir(g.Common)
}

// tops are the top folders of every worktree of the repository that is on
// disk, the main one first, as git lists them in <common>/worktrees/*/gitdir.
func (g gitWhere) tops() []string {
	var out []string
	if m := g.mainTop(); m != "" {
		out = append(out, m)
	}
	es, _ := os.ReadDir(filepath.Join(g.Common, "worktrees"))
	var more []string
	for _, e := range es {
		b, err := os.ReadFile(filepath.Join(g.Common, "worktrees", e.Name(), "gitdir"))
		if err != nil {
			continue
		}
		p := strings.TrimSpace(string(b))
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(g.Common, "worktrees", e.Name(), p)
		}
		top := filepath.Dir(filepath.Clean(p))
		if fi, err := os.Stat(top); err == nil && fi.IsDir() && !slices.Contains(out, top) && !slices.Contains(more, top) {
			more = append(more, top)
		}
	}
	sort.Strings(more)
	return append(out, more...)
}

// treeDirs are the project's folder in each other worktree of its
// repository that has it: the same folder under each worktree's top.
func (p *Project) treeDirs() []string {
	g, ok := gitOf(p.Dir)
	if !ok {
		return nil
	}
	rel, err := filepath.Rel(g.Top, p.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, top := range g.tops() {
		if top == g.Top || realDir(top) == realDir(g.Top) {
			continue
		}
		d := filepath.Join(top, rel)
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// projectKey is the short name a project is known to the router by: the
// hub's address for its agents ends in it (HubProjectSep).
func projectKey(dir string) string {
	h := sha256.Sum256([]byte(filepath.Clean(dir)))
	return hex.EncodeToString(h[:6])
}

// excludeBegin is the first of the lines magpie keeps a project's entries
// between in info/exclude, one block per project (several can share a
// repository); ignoreEnd ends it.
func excludeBegin(dir string) string {
	return "# >>> magpie: library items placed in " + filepath.ToSlash(dir) + " (magpie adds and removes these lines) >>>"
}

// writeExclude keeps the repository's info/exclude listing the entries
// magpie placed in the project, relative to its worktree's top, between
// the project's two lines; every other line is git's or the user's.
func (p *Project) writeExclude(g gitWhere, entries []string) error {
	f := filepath.Join(g.Common, "info", "exclude")
	b, err := os.ReadFile(f)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	rel, err := filepath.Rel(g.Top, p.Dir)
	if err != nil {
		return err
	}
	pre := "/"
	if rel = filepath.ToSlash(rel); rel != "." {
		pre += rel + "/"
	}
	nl := "\n"
	if strings.Contains(string(b), "\r\n") {
		nl = "\r\n"
	}
	begin := excludeBegin(p.Dir)
	out := stripBlock(string(b), begin)
	if len(entries) > 0 {
		sort.Strings(entries)
		if out != "" {
			if !strings.HasSuffix(out, "\n") {
				out += nl
			}
			out += nl
		}
		out += begin + nl
		for _, e := range slices.Compact(entries) {
			out += pre + e + nl
		}
		out += ignoreEnd + nl
	}
	if out == string(b) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		return err
	}
	return edit.WriteAtomic(f, []byte(out))
}

// stripBlock removes complete managed blocks and their separator, keeping
// the other lines byte for byte, including their line endings.
func stripBlock(s, begin string) string {
	lines := strings.SplitAfter(s, "\n")
	var out strings.Builder
	start := 0
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != begin {
			continue
		}
		end := i + 1
		for ; end < len(lines); end++ {
			line := strings.TrimSpace(lines[end])
			if line == ignoreEnd || strings.HasPrefix(line, "# >>> magpie:") {
				break
			}
		}
		if end == len(lines) || strings.TrimSpace(lines[end]) != ignoreEnd {
			continue
		}
		stop := i
		if i > start && (lines[i-1] == "\n" || lines[i-1] == "\r\n") {
			stop--
		}
		for _, line := range lines[start:stop] {
			out.WriteString(line)
		}
		start, i = end+1, end
	}
	if start == 0 {
		return s
	}
	for _, line := range lines[start:] {
		out.WriteString(line)
	}
	return out.String()
}
