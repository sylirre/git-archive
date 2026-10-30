// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const giteaPageSize = 50 // the default server-side maximum

type gitea struct{ api *api }

type gtUser struct {
	Login    string `json:"login"`
	Username string `json:"username"`
}

type gtRepo struct {
	ID       int64    `json:"id"`
	FullName string   `json:"full_name"`
	Private  bool     `json:"private"`
	Fork     bool     `json:"fork"`
	Archived bool     `json:"archived"`
	Empty    bool     `json:"empty"`
	Size     int64    `json:"size"` // KiB
	Default  string   `json:"default_branch"`
	HasWiki  bool     `json:"has_wiki"`
	Desc     string   `json:"description"`
	Website  string   `json:"website"`
	HTMLURL  string   `json:"html_url"`
	Language string   `json:"language"`
	Stars    int      `json:"stars_count"`
	Forks    int      `json:"forks_count"`
	Topics   []string `json:"topics"`
	Created  string   `json:"created_at"`
	Updated  string   `json:"updated_at"`
}

func (g *gitea) owner(ctx context.Context, name string) (string, error) {
	var u gtUser
	if _, err := g.api.get(ctx, g.api.endpoint(nil, "users", name), &u); err != nil {
		return "", fmt.Errorf("user or organization %q: %w", name, err)
	}
	login := u.Login
	if login == "" {
		login = u.Username
	}
	if err := checkPath(login); err != nil {
		return "", err
	}
	if !strings.EqualFold(login, name) {
		return "", fmt.Errorf("server returned owner %q for %q", login, name)
	}
	return login, nil
}

func (g *gitea) Resolve(ctx context.Context, s Spec) (Spec, error) {
	if s.Type == TypeOwner {
		login, err := g.owner(ctx, s.Path)
		if err != nil {
			return s, err
		}
		s.Path = login
	} else {
		r, err := g.Get(ctx, s)
		if err != nil {
			return s, err
		}
		s.Path = r.Path
	}
	s.URL = canonicalURL(s)
	return s, nil
}

func (g *gitea) Get(ctx context.Context, s Spec) (Repo, error) {
	owner, name, _ := strings.Cut(s.Path, "/")
	var raw gtRepo
	if _, err := g.api.get(ctx, g.api.endpoint(nil, "repos", owner, name), &raw); err != nil {
		return Repo{}, fmt.Errorf("repository %q: %w", s.Path, err)
	}
	return g.convert(s, owner, raw)
}

func (g *gitea) List(ctx context.Context, s Spec) (Listing, error) {
	login, err := g.owner(ctx, s.Path)
	if err != nil {
		return Listing{}, err
	}
	q := url.Values{"limit": {strconv.Itoa(giteaPageSize)}, "page": {"1"}}
	// Organisations and users share a namespace; try the organisation
	// endpoint first, as it includes private repositories for members.
	u := g.api.endpoint(q, "orgs", login, "repos")
	var probe []gtRepo
	if _, err := g.api.get(ctx, u, &probe); errors.Is(err, ErrNotFound) {
		u = g.api.endpoint(q, "users", login, "repos")
	} else if err != nil {
		return Listing{}, err
	}
	next := func(cur *url.URL, n int) *url.URL {
		if n < giteaPageSize {
			return nil
		}
		q := cur.Query()
		p, _ := strconv.Atoi(q.Get("page"))
		q.Set("page", strconv.Itoa(p+1))
		nu := *cur
		nu.RawQuery = q.Encode()
		return &nu
	}
	var l Listing
	err = paginate(ctx, g.api, u, func(items []gtRepo) error {
		for _, it := range items {
			r, err := g.convert(s, login, it)
			if err != nil {
				l.Skipped = append(l.Skipped, err.Error())
				continue
			}
			l.add(r)
		}
		return nil
	}, next)
	return l, err
}

func (g *gitea) convert(s Spec, owner string, it gtRepo) (Repo, error) {
	r, err := newRepo(s, owner, it.FullName, g.api.protocol)
	if err != nil {
		return r, err
	}
	r.ForgeID = strconv.FormatInt(it.ID, 10)
	r.DefaultBranch = it.Default
	r.Fork, r.Archived, r.Private, r.Empty = it.Fork, it.Archived, it.Private, it.Empty
	r.Size = it.Size * 1024
	if it.HasWiki {
		r.WikiURL = wikiURL(r.CloneURL)
	}
	r.Meta = &Metadata{
		Provider: Gitea, ID: r.ForgeID, FullName: it.FullName, Description: it.Desc, Homepage: it.Website,
		WebURL: it.HTMLURL, Language: it.Language, Topics: it.Topics, Stars: it.Stars, Forks: it.Forks,
		Fork: it.Fork, Archived: it.Archived, Private: it.Private, DefaultBranch: it.Default, HasWiki: it.HasWiki,
		Size: r.Size, Created: it.Created, Updated: it.Updated,
	}
	return r, nil
}
