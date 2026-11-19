package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/runtimes"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Managed runtime catalog.
//
// Admins publish signed runtime packages; the router stores them (in the
// settings table) and hands them to agents, which verify the signature
// against keys pinned in their own config before installing anything. The
// router only checks that a package is well formed and that its signature
// was made for that package name and version.

const settingRuntimeCatalog = "runtime_catalog"

// CatalogBlobTTL is how long a package download URL handed to an agent
// stays valid.
const CatalogBlobTTL = 6 * time.Hour

func (e *Engine) loadCatalog(ctx context.Context) {
	v, err := e.st.GetSetting(ctx, settingRuntimeCatalog)
	if err != nil {
		return
	}
	var c api.RuntimeCatalog
	if json.Unmarshal([]byte(v), &c) == nil {
		e.catalog = c
	}
}

// saveCatalogLocked bumps the revision and persists the catalog.
func (e *Engine) saveCatalogLocked(ctx context.Context, c api.RuntimeCatalog) error {
	rev := ms(e.now())
	if rev <= e.catalog.Revision {
		rev = e.catalog.Revision + 1
	}
	c.Revision = rev
	sort.Slice(c.Packages, func(i, j int) bool { return c.Packages[i].Package.Name < c.Packages[j].Package.Name })
	if err := e.st.PutSetting(ctx, settingRuntimeCatalog, jsonStr(c)); err != nil {
		return err
	}
	e.catalog = c
	return nil
}

func copyCatalog(c api.RuntimeCatalog) api.RuntimeCatalog {
	out := api.RuntimeCatalog{Revision: c.Revision, Packages: append([]api.RuntimeCatalogEntry{}, c.Packages...)}
	return out
}

// PublishRuntime adds a signed package to the catalog or replaces the
// package of the same name (a new version, or the same version re-signed).
func (e *Engine) PublishRuntime(ctx context.Context, req api.RuntimePublish, by string) (*api.RuntimeCatalogEntry, error) {
	p, keyID, err := runtimes.CheckSignature([]byte(req.Manifest), []byte(req.Signature))
	if err != nil {
		return nil, &ValidationError{err.Error()}
	}
	e.mu.Lock()
	c := copyCatalog(e.catalog)
	entry := api.RuntimeCatalogEntry{Package: *p, Manifest: req.Manifest, Signature: req.Signature, KeyID: keyID,
		Prewarm: req.Prewarm, PublishedAt: e.now().UTC(), PublishedBy: by}
	prev := ""
	replaced := false
	for i, x := range c.Packages {
		if x.Package.Name == p.Name {
			prev = x.Package.Version
			c.Packages[i] = entry
			replaced = true
		}
	}
	if !replaced {
		c.Packages = append(c.Packages, entry)
	}
	// One command, one package: the scheduler reasons about commands.
	for _, x := range c.Packages {
		if x.Package.Name == p.Name {
			continue
		}
		for cmd := range p.Commands {
			if _, dup := x.Package.Commands[cmd]; dup {
				e.mu.Unlock()
				return nil, &ValidationError{fmt.Sprintf("command %q is already provided by package %s", cmd, x.Package.Name)}
			}
		}
	}
	err = e.saveCatalogLocked(ctx, c)
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	msg := "published runtime " + p.Name + " " + p.Version
	if prev != "" && prev != p.Version {
		msg += " (replaces " + prev + ")"
	}
	if req.Prewarm {
		msg += ", prewarm on"
	}
	e.Audit(ctx, api.EventRuntimeCatalog, by, p.Name, msg)
	e.log.Info("runtime catalog: package published", "package", p.Name, "version", p.Version, "key", keyID, "by", by)
	return &entry, nil
}

// DeleteRuntime removes a package from the catalog. Macs remove their copy
// once no task uses it.
func (e *Engine) DeleteRuntime(ctx context.Context, name, by string) error {
	e.mu.Lock()
	c := copyCatalog(e.catalog)
	out := c.Packages[:0]
	found := false
	for _, x := range c.Packages {
		if x.Package.Name == name {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		e.mu.Unlock()
		return ErrNotFound
	}
	c.Packages = out
	err := e.saveCatalogLocked(ctx, c)
	e.mu.Unlock()
	if err == nil {
		e.Audit(ctx, api.EventRuntimeCatalog, by, name, "removed runtime "+name+" from the catalog")
	}
	return err
}

// SetRuntimePrewarm turns fleet-wide pre-installation on or off.
func (e *Engine) SetRuntimePrewarm(ctx context.Context, name string, on bool, by string) error {
	e.mu.Lock()
	c := copyCatalog(e.catalog)
	found := false
	for i := range c.Packages {
		if c.Packages[i].Package.Name == name {
			c.Packages[i].Prewarm, found = on, true
		}
	}
	if !found {
		e.mu.Unlock()
		return ErrNotFound
	}
	err := e.saveCatalogLocked(ctx, c)
	e.mu.Unlock()
	if err == nil {
		state := "off"
		if on {
			state = "on"
		}
		e.Audit(ctx, api.EventRuntimeCatalog, by, name, "prewarm "+state+" for runtime "+name)
	}
	return err
}

// RuntimeCatalog returns the catalog with each package's state across the
// fleet.
func (e *Engine) RuntimeCatalog(ctx context.Context) (api.RuntimeCatalog, error) {
	agents, err := e.st.ListAgents(ctx)
	if err != nil {
		return api.RuntimeCatalog{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c := copyCatalog(e.catalog)
	now := e.now()
	for i := range c.Packages {
		p := &c.Packages[i]
		p.Macs = []api.RuntimeMacStatus{}
		for _, a := range agents {
			l := e.live[a.ID]
			if l == nil || !e.online(a.ID, now) {
				continue
			}
			s := api.RuntimeMacStatus{ID: a.ID, Name: a.Name}
			for _, ps := range l.hb.Packages {
				if ps.Name != p.Package.Name {
					continue
				}
				s.Version, s.Error = ps.Version, ps.Error
				s.State = ps.State
				if ps.State == api.PackageInstalled && ps.Version != p.Package.Version {
					s.State = api.PackageOutdated
				}
			}
			if s.State == "" || s.State == api.PackageOutdated {
				for _, n := range l.hb.InstallablePackages {
					if n == p.Package.Name {
						s.State = api.PackageInstallable
						if s.Version == p.Package.Version {
							s.Version = ""
						}
					}
				}
			}
			if s.State != "" {
				p.Macs = append(p.Macs, s)
			}
		}
		sort.Slice(p.Macs, func(i, j int) bool { return strings.ToLower(p.Macs[i].Name) < strings.ToLower(p.Macs[j].Name) })
	}
	return c, nil
}

// AgentRuntimeCatalog is the catalog as agents get it, with blob artifacts
// resolved to short-lived download URLs.
func (e *Engine) AgentRuntimeCatalog(ctx context.Context) (api.AgentRuntimeCatalog, error) {
	e.mu.Lock()
	c := copyCatalog(e.catalog)
	e.mu.Unlock()
	out := api.AgentRuntimeCatalog{Revision: c.Revision, Packages: []api.AgentRuntimePackage{}}
	for _, x := range c.Packages {
		ap := api.AgentRuntimePackage{Manifest: x.Manifest, Signature: x.Signature, Prewarm: x.Prewarm,
			URLs: make([]string, len(x.Package.Artifacts))}
		for i, a := range x.Package.Artifacts {
			if a.Blob == "" {
				ap.URLs[i] = a.URL
				continue
			}
			u, err := e.blobs.DownloadURL(ctx, UploadKey(a.Blob), CatalogBlobTTL)
			if err != nil {
				return out, err
			}
			ap.URLs[i] = u
		}
		out.Packages = append(out.Packages, ap)
	}
	return out, nil
}

// catalogRevisionLocked is what heartbeats tell agents.
func (e *Engine) catalogRevisionLocked() int64 { return e.catalog.Revision }

// readyPackagesLocked returns the packages a Mac has installed at the
// catalog's current version.
func (e *Engine) readyPackagesLocked(hb api.Heartbeat) map[string]bool {
	cur := map[string]string{}
	for _, x := range e.catalog.Packages {
		cur[x.Package.Name] = x.Package.Version
	}
	out := map[string]bool{}
	for _, p := range hb.Packages {
		if p.State == api.PackageInstalled && cur[p.Name] == p.Version {
			out[p.Name] = true
		}
	}
	return out
}
