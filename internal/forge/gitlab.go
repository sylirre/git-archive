package forge

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type gitlab struct{ api *api }

type glProject struct {
	ID          int64  `json:"id"`
	Path        string `json:"path_with_namespace"`
	Default     string `json:"default_branch"`
	Archived    bool   `json:"archived"`
	Visibility  string `json:"visibility"`
	Empty       bool   `json:"empty_repo"`
	WikiEnabled *bool  `json:"wiki_enabled"`
	WikiAccess  string `json:"wiki_access_level"`
	ForkedFrom  *struct {
		ID int64 `json:"id"`
	} `json:"forked_from_project"`
	Desc       string   `json:"description"`
	WebURL     string   `json:"web_url"`
	Stars      int      `json:"star_count"`
	Forks      int      `json:"forks_count"`
	Topics     []string `json:"topics"`
	TagList    []string `json:"tag_list"`
	Created    string   `json:"created_at"`
	Activity   string   `json:"last_activity_at"`
	Statistics *struct {
		RepositorySize int64 `json:"repository_size"`
	} `json:"statistics"`
}

type glGroup struct {
	ID       int64  `json:"id"`
	FullPath string `json:"full_path"`
}

type glUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// namespace looks a path up as a group, then as a user.
func (g *gitlab) namespace(ctx context.Context, path string) (u *url.URL, owner string, err error) {
	q := url.Values{"per_page": {"100"}, "order_by": {"id"}, "sort": {"asc"}}
	var grp glGroup
	_, err = g.api.get(ctx, g.api.endpoint(nil, "groups", path), &grp)
	if err == nil {
		q.Set("include_subgroups", "true")
		q.Set("with_shared", "false")
		return g.api.endpoint(q, "groups", strconv.FormatInt(grp.ID, 10), "projects"), grp.FullPath, nil
	}
	if !errors.Is(err, ErrNotFound) || strings.Contains(path, "/") {
		return nil, "", fmt.Errorf("GitLab group %q: %w", path, err)
	}
	var users []glUser
	if _, err := g.api.get(ctx, g.api.endpoint(url.Values{"username": {path}}, "users"), &users); err != nil {
		return nil, "", fmt.Errorf("GitLab user %q: %w", path, err)
	}
	if len(users) != 1 || checkPath(users[0].Username) != nil || !strings.EqualFold(users[0].Username, path) {
		return nil, "", fmt.Errorf("GitLab group or user %q: %w", path, ErrNotFound)
	}
	return g.api.endpoint(q, "users", strconv.FormatInt(users[0].ID, 10), "projects"), users[0].Username, nil
}

func (g *gitlab) Resolve(ctx context.Context, s Spec) (Spec, error) {
	if s.Type != TypeOwner {
		r, err := g.Get(ctx, s)
		if err == nil {
			s.Type, s.Path = TypeRepo, r.Path
			s.URL = canonicalURL(s)
			return s, nil
		}
		if s.Type == TypeRepo || !errors.Is(err, ErrNotFound) {
			return s, err
		}
	}
	_, owner, err := g.namespace(ctx, s.Path)
	if err != nil {
		return s, err
	}
	if err := checkPath(owner); err != nil {
		return s, err
	}
	s.Type, s.Path = TypeOwner, owner
	s.URL = canonicalURL(s)
	return s, nil
}

func (g *gitlab) Get(ctx context.Context, s Spec) (Repo, error) {
	var p glProject
	if _, err := g.api.get(ctx, g.api.endpoint(nil, "projects", s.Path), &p); err != nil {
		return Repo{}, fmt.Errorf("GitLab project %q: %w", s.Path, err)
	}
	return g.convert(s, "", p)
}

func (g *gitlab) List(ctx context.Context, s Spec) (Listing, error) {
	u, owner, err := g.namespace(ctx, s.Path)
	if err != nil {
		return Listing{}, err
	}
	var l Listing
	err = paginate(ctx, g.api, u, func(items []glProject) error {
		for _, it := range items {
			r, err := g.convert(s, owner, it)
			if err != nil {
				l.Skipped = append(l.Skipped, err.Error())
				continue
			}
			l.add(r)
		}
		return nil
	}, nil)
	return l, err
}

func (g *gitlab) convert(s Spec, owner string, it glProject) (Repo, error) {
	r, err := newRepo(s, owner, it.Path, g.api.protocol)
	if err != nil {
		return r, err
	}
	r.ForgeID = strconv.FormatInt(it.ID, 10)
	r.DefaultBranch = it.Default
	r.Archived = it.Archived
	r.Private = it.Visibility != "" && it.Visibility != "public"
	r.Fork = it.ForkedFrom != nil
	r.Empty = it.Empty
	if it.Statistics != nil {
		r.Size = it.Statistics.RepositorySize
	}
	wiki := it.WikiAccess == "enabled" || it.WikiAccess == "private" || (it.WikiAccess == "" && it.WikiEnabled != nil && *it.WikiEnabled)
	if wiki {
		r.WikiURL = wikiURL(r.CloneURL)
	}
	topics := it.Topics
	if len(topics) == 0 {
		topics = it.TagList
	}
	r.Meta = &Metadata{
		Provider: GitLab, ID: r.ForgeID, FullName: it.Path, Description: it.Desc, WebURL: it.WebURL,
		Topics: topics, Stars: it.Stars, Forks: it.Forks, Fork: r.Fork, Archived: it.Archived, Private: r.Private,
		DefaultBranch: it.Default, HasWiki: wiki, Size: max(r.Size, 0), Created: it.Created, Updated: it.Activity,
	}
	return r, nil
}
