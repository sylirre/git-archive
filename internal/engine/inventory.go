package engine

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sylirre/git-archive/internal/archive"
)

// Entry describes one repository known to the archive.
type Entry struct {
	ID         string    `json:"id"`
	Source     string    `json:"source,omitempty"`
	Status     string    `json:"status"`
	Detail     string    `json:"detail,omitempty"`
	OnDisk     bool      `json:"onDisk"`
	Wiki       bool      `json:"wiki,omitempty"`
	Fork       bool      `json:"fork,omitempty"`
	Archived   bool      `json:"archived,omitempty"`
	Size       int64     `json:"size,omitempty"`
	Preserved  int       `json:"preserved,omitempty"`
	LastSync   time.Time `json:"lastSync,omitzero"`
	LastVerify time.Time `json:"lastVerify,omitzero"`
}

// Inventory lists repositories from the state and from disk, without
// network access. Partial clones left behind are returned separately.
func (e *Engine) Inventory(only []string) (entries []Entry, partials []string) {
	a := e.Archive
	onDisk, partials := scanRepos(a.Root, a.Config.Layout)
	ids := map[string]bool{}
	for id := range a.State.Repos {
		ids[id] = true
	}
	for id := range onDisk {
		ids[id] = true
	}
	for id := range ids {
		if !archive.MatchAny(only, id) {
			continue
		}
		entries = append(entries, e.entry(id, onDisk[id] != ""))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries, partials
}

func (e *Engine) entry(id string, disk bool) Entry {
	cfg := e.Archive.Config
	en := Entry{ID: id, OnDisk: disk}
	rs := e.Archive.State.Repos[id]
	if rs == nil {
		en.Status, en.Detail = "unmanaged", "on disk but not known to git-archive"
		return en
	}
	en.Source, en.Wiki, en.Fork, en.Archived, en.Size = rs.Source, rs.Wiki, rs.Fork, rs.Archived, rs.Size
	en.Preserved, en.LastSync, en.LastVerify = rs.Preserved, rs.LastSync, rs.LastVerify
	src := cfg.Source(rs.Source)
	opts, _ := cfg.Options(src)
	excluded, by := archive.Excluded(cfg.Excludes, id)
	switch {
	case src == nil:
		en.Status, en.Detail = "orphaned", "its source was removed from the configuration"
	case !rs.Gone.IsZero():
		en.Status, en.Detail = "gone", "missing upstream since "+rs.Gone.Local().Format("2006-01-02")
	case excluded:
		en.Status, en.Detail = "excluded", "pattern "+by
	case rs.Fork && opts.SkipForks:
		en.Status, en.Detail = "filtered", "fork (skipForks)"
	case rs.Archived && opts.SkipArchived:
		en.Status, en.Detail = "filtered", "archived upstream (skipArchived)"
	case opts.MaxSize > 0 && rs.Size > opts.MaxSize:
		en.Status, en.Detail = "filtered", "larger than maxSize"
	case !rs.Absent.IsZero() && !disk:
		en.Status, en.Detail = "absent", "no wiki upstream"
	case rs.LastError != "":
		en.Status, en.Detail = "failed", rs.LastError
	case !disk && (!rs.Cloned.IsZero() || !rs.LastSync.IsZero()):
		en.Status, en.Detail = "missing", "previously archived, no longer on disk"
	case !disk:
		en.Status = "pending"
	default:
		en.Status = "ok"
	}
	if en.Status == "ok" && rs.VerifyError != "" {
		en.Status, en.Detail = "corrupt", rs.VerifyError
	}
	return en
}

// scanRepos finds repositories below root without descending into them.
// It returns IDs mapped to directories, and leftover partial clones.
func scanRepos(root, layout string) (map[string]string, []string) {
	found := map[string]string{}
	var partials []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		if rel == archive.MetaDir {
			return filepath.SkipDir
		}
		depth := strings.Count(filepath.ToSlash(rel), "/") + 1
		if strings.HasSuffix(d.Name(), archive.PartialSuffix) {
			partials = append(partials, p)
			return filepath.SkipDir
		}
		if depth >= 2 && isRepo(p) {
			id := filepath.ToSlash(rel)
			host, path := archive.SplitID(id)
			host = strings.ReplaceAll(host, "_", ":") // inverse of safe.HostDir
			if layout == archive.LayoutBare {
				path = strings.TrimSuffix(path, ".git")
			}
			found[host+"/"+path] = p
			return filepath.SkipDir
		}
		if depth > 12 {
			return filepath.SkipDir
		}
		return nil
	})
	return found, partials
}
