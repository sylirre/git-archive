package forge

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type github struct{ api *api }

type ghOwner struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type ghRepo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	Fork     bool   `json:"fork"`
	Archived bool   `json:"archived"`
	Size     int64  `json:"size"` // KiB
	Default  string `json:"default_branch"`
	HasWiki  bool   `json:"has_wiki"`
	Desc     string `json:"description"`
	Homepage string `json:"homepage"`
	HTMLURL  string `json:"html_url"`
	Language string `json:"language"`
	License  *struct {
		SPDX string `json:"spdx_id"`
	} `json:"license"`
	Topics  []string `json:"topics"`
	Stars   int      `json:"stargazers_count"`
	Forks   int      `json:"forks_count"`
	Created string   `json:"created_at"`
	Updated string   `json:"updated_at"`
	Pushed  string   `json:"pushed_at"`
}

func (g *github) owner(ctx context.Context, name string) (ghOwner, error) {
	var o ghOwner
	if _, err := g.api.get(ctx, g.api.endpoint(nil, "users", name), &o); err != nil {
		return o, fmt.Errorf("GitHub user or organization %q: %w", name, err)
	}
	if err := checkPath(o.Login); err != nil {
		return o, err
	}
	if !strings.EqualFold(o.Login, name) {
		return o, fmt.Errorf("GitHub returned owner %q for %q", o.Login, name)
	}
	return o, nil
}

func (g *github) Resolve(ctx context.Context, s Spec) (Spec, error) {
	if s.Type == TypeOwner {
		o, err := g.owner(ctx, s.Path)
		if err != nil {
			return s, err
		}
		s.Path = o.Login
	} else {
		r, err := g.Get(ctx, s)
		if err != nil {
			return s, err
		}
		s.Path = r.Path
	}
	s.URL = s.Scheme + "://" + s.Host + "/" + s.Path
	return s, nil
}

func (g *github) Get(ctx context.Context, s Spec) (Repo, error) {
	owner, name, _ := strings.Cut(s.Path, "/")
	var raw ghRepo
	if _, err := g.api.get(ctx, g.api.endpoint(nil, "repos", owner, name), &raw); err != nil {
		return Repo{}, fmt.Errorf("GitHub repository %q: %w", s.Path, err)
	}
	return g.convert(s, owner, raw)
}

func (g *github) List(ctx context.Context, s Spec) (Listing, error) {
	o, err := g.owner(ctx, s.Path)
	if err != nil {
		return Listing{}, err
	}
	q := url.Values{"per_page": {"100"}}
	var u *url.URL
	if o.Type == "Organization" {
		q.Set("type", "all")
		u = g.api.endpoint(q, "orgs", o.Login, "repos")
	} else {
		q.Set("type", "owner")
		u = g.api.endpoint(q, "users", o.Login, "repos")
	}
	var l Listing
	err = paginate(ctx, g.api, u, func(items []ghRepo) error {
		for _, it := range items {
			r, err := g.convert(s, o.Login, it)
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

func (g *github) convert(s Spec, owner string, it ghRepo) (Repo, error) {
	r, err := newRepo(s, owner, it.FullName, g.api.protocol)
	if err != nil {
		return r, err
	}
	r.ForgeID = strconv.FormatInt(it.ID, 10)
	r.DefaultBranch = it.Default
	r.Fork, r.Archived, r.Private = it.Fork, it.Archived, it.Private
	r.Size = it.Size * 1024
	if it.HasWiki {
		r.WikiURL = wikiURL(r.CloneURL)
	}
	m := &Metadata{
		Provider: GitHub, ID: r.ForgeID, FullName: it.FullName, Description: it.Desc, Homepage: it.Homepage,
		WebURL: it.HTMLURL, Language: it.Language, Topics: it.Topics, Stars: it.Stars, Forks: it.Forks,
		Fork: it.Fork, Archived: it.Archived, Private: it.Private, DefaultBranch: it.Default, HasWiki: it.HasWiki,
		Size: r.Size, Created: it.Created, Updated: it.Updated, Pushed: it.Pushed,
	}
	if it.License != nil && it.License.SPDX != "NOASSERTION" {
		m.License = it.License.SPDX
	}
	r.Meta = m
	return r, nil
}
