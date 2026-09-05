package compose

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// stripUnreadableIncludes rewrites data (the raw content of the compose
// file at path, already known to be readable — it is one of Load's own
// main config files) with any `include:` entry whose target cannot be
// read or parsed removed, recursively: an include target that DOES read
// and parse cleanly is itself walked the same way, so an unreadable file
// several includes deep is tolerated exactly like one at the top. It
// reports whether anything changed anywhere in the subtree and the full
// set of temp files it staged (see writeRewrittenInclude) so the caller
// can clean all of them up, not just the first level's.
//
// Two failure modes are tolerated, matching everything the bespoke
// loader's skipUnreadableIncludes parameter tolerated (it wrapped any
// error the recursive loadComposeTree call produced — a missing file, a
// YAML parse failure, the size cap, or a cycle — indiscriminately):
// - the file cannot be opened (missing, permission denied, or over
// MaxUserFileBytes), and
// - the file opens but does not parse as YAML (e.g. an editor mid-save
// on a shared included file is at least as likely as the file having
// been deleted).
//
// A cycle back to a file already being processed in the current include
// chain is treated the same way: dropped, not propagated as an error —
// again matching the bespoke loader's behavior under skipUnreadableIncludes.
//
// This exists for reloadProject: the compose file a live project was
// started from can drift under it (an `include:`d file is typically a
// *shared* one, living outside the project directory, and can move,
// disappear, or be mid-edit without the project's own file changing), and
// a reload runs on every supervisor tick. Failing the whole load over one
// stale include — at any depth — would stop the supervisor restarting
// every other service in the project for as long as the drift lasts.
func stripUnreadableIncludes(path string, data []byte, warn io.Writer) (rewritten []byte, changed bool, staged []string, err error) {
	if warn == nil {
		warn = io.Discard
	}
	// Absolutized once, here, so every path this whole recursion computes
	// from here on — the chain guard's keys, each level's base directory,
	// every staged temp file's location — is consistently absolute. Load's
	// one caller (reloadProject) always hands this function an absolute
	// path already, but nothing in this package enforces that precondition,
	// and a relative path here (relative to the process's actual cwd, not
	// necessarily Options.WorkingDir) would let a later filepath.Join
	// silently double up a path segment instead of producing the path the
	// caller meant.
	if abs, aerr := filepath.Abs(path); aerr == nil {
		path = abs
	}
	chain := map[string]bool{filepath.Clean(path): true}
	var stagedFiles []string
	out, ch, err := stripIncludesAtLevel(path, data, chain, warn, &stagedFiles)
	if err != nil {
		for _, f := range stagedFiles {
			_ = os.Remove(f)
		}
		return nil, false, nil, err
	}
	return out, ch, stagedFiles, nil
}

// stripIncludesAtLevel does the actual work for one file's already-read
// content: parse it, find its own `include:` list (if any), resolve and
// recursively process every entry's target(s) via resolveIncludeTarget,
// and drop any entry where that resolution failed. It also checks every
// service declared directly in this file for an `extends: {file: ...}`
// target (see stripUnreadableExtends) and drops that one service's
// `extends` when the target is unreadable or unparseable — the same
// tolerant-drop this function already applies to `include`, and at the
// same one-level scope: an extends target reached through a chain of
// includes gets checked too (this function runs once per file the include
// recursion visits), but an extends target's OWN extends chain is not
// walked any further. staged accumulates every temp file created anywhere
// in the recursion so the top-level caller can remove them all once Load
// is done.
func stripIncludesAtLevel(path string, data []byte, chain map[string]bool, warn io.Writer, staged *[]string) (rewritten []byte, changed bool, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		// A parse error in the file *this* call was handed is the caller's
		// problem to detect (resolveIncludeTarget does, via its own parse
		// attempt) — by the time content reaches here it's assumed to
		// already parse, so this is defensive, not a normal path.
		return data, false, nil
	}
	root := asMapping(&doc)
	if root == nil {
		return data, false, nil
	}
	base := filepath.Dir(path)
	var anyChange bool

	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "include" {
			continue
		}
		seq := root.Content[i+1]
		if seq.Kind != yaml.SequenceNode {
			break // malformed; compose-go reports it, leave include alone
		}
		var kept []*yaml.Node
		var includeChanged bool
		for _, item := range seq.Content {
			ok, itemChanged := resolveIncludeEntry(item, base, chain, warn, staged)
			if !ok {
				includeChanged = true
				continue // drop the whole entry
			}
			if itemChanged {
				includeChanged = true
			}
			kept = append(kept, item)
		}
		if includeChanged {
			anyChange = true
			if len(kept) == 0 {
				// Drop the "include" key entirely: it and its value sit at
				// [i, i+1] in the flat key/value Content slice.
				root.Content = append(root.Content[:i], root.Content[i+2:]...)
			} else {
				seq.Content = kept
			}
		}
		break
	}

	if stripUnreadableExtends(root, base, warn) {
		anyChange = true
	}

	if !anyChange {
		return data, false, nil
	}
	out, merr := yaml.Marshal(&doc)
	if merr != nil {
		return nil, false, merr
	}
	return out, true, nil
}

// stripUnreadableExtends removes a service's `extends: {file: ...}` entry
// from root's `services:` mapping when that file cannot be opened, is over
// the size cap, or does not parse as YAML — the same tolerant-drop
// stripIncludesAtLevel already applies to an unreadable `include:` entry,
// for the same reason: reloadProject calls Load on every supervisor tick,
// and an extends base file (commonly a *shared* file living outside the
// project directory) moving, disappearing, or being mid-edit must not fail
// the whole reload and stop every other service in the project from being
// restarted for as long as the drift lasts.
//
// Scope matches load.go's size-cap/warn-walk extension to extends targets:
// one level. Only the direct `extends.file` a service in root names is
// checked; that target's own content is never parsed beyond confirming it
// opens and parses as YAML, so a service whose extends target itself
// extends (or includes) something broken is not covered here — the
// deeper chain is a separately-tracked, still-deferred gap, same as it is
// for include. The short scalar form (`extends: SERVICE`, a same-file
// reference) names no file and is left untouched: there is nothing to
// verify.
func stripUnreadableExtends(root *yaml.Node, base string, warn io.Writer) (changed bool) {
	var services *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "services" {
			services = root.Content[i+1]
			break
		}
	}
	if services == nil || services.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		svc := services.Content[i+1]
		if svc.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(svc.Content); j += 2 {
			if svc.Content[j].Value != "extends" {
				continue
			}
			ext := svc.Content[j+1]
			if ext.Kind != yaml.MappingNode {
				break // short scalar form; same-file extends, nothing to check
			}
			var file string
			for k := 0; k+1 < len(ext.Content); k += 2 {
				if ext.Content[k].Value == "file" {
					file = ext.Content[k+1].Value
				}
			}
			if file == "" {
				break // no 'file' key; same-file extends
			}
			p := file
			if !filepath.IsAbs(p) {
				p = filepath.Join(base, p)
			}
			if abs, err := filepath.Abs(p); err == nil {
				p = abs
			} else {
				p = filepath.Clean(p)
			}
			if err := fileReadsAndParses(p); err != nil {
				composeWarn(warn, "extends %s: skipped, the service definition it provides is not in this reload (%v)", p, err)
				svc.Content = append(svc.Content[:j], svc.Content[j+2:]...)
				changed = true
			}
			break
		}
	}
	return changed
}

// fileReadsAndParses reports whether p can be opened, read within the size
// cap, and parses as YAML — the same two failure modes
// stripUnreadableIncludes documents as tolerated (missing/oversized, or a
// parse failure from an editor mid-save), checked here with no recursion
// into whatever p itself references.
func fileReadsAndParses(p string) error {
	data, err := readCappedFile(p)
	if err != nil {
		return err
	}
	var probe yaml.Node
	return yaml.Unmarshal(data, &probe)
}

// capTopLevelIncludes size-caps every path named in a main config file's
// top-level `include:` list — read from data, the file's already-in-hand
// raw bytes — before compose-go itself gets a chance to read them.
// cli.NewProjectOptions only resolves the main file(s) up front; the actual
// reading of an include target's content happens, unbounded, deep inside
// compose-go's own ApplyInclude the moment po.LoadProject runs. Load's
// post-hoc cap (see the referencedFiles loop there) cannot stop that read
// from completing first, so for a top-level include: entry this pre-pass
// is the only thing that can — pointing `include:` at an endless stream
// like /dev/zero hangs po.LoadProject forever without it.
//
// This runs unconditionally (both with and without
// Options.SkipUnreadableIncludes): when the flag is set,
// stripUnreadableIncludes already performs an equivalent pre-read cap as
// part of its own tolerant walk, so Load only calls this in the plain
// (non-tolerant) case — see the call site. It stays one level deep, same
// as the include-tolerance walk's documented scope for this exact
// concern: a nested include, or any extends target (top-level or not), is
// still only capped after compose-go has already read it, a real,
// documented gap this pre-pass does not attempt to close.
func capTopLevelIncludes(mainPath string, data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil // malformed; compose-go reports it
	}
	root := asMapping(&doc)
	if root == nil {
		return nil
	}
	base := filepath.Dir(mainPath)
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "include" {
			continue
		}
		seq := root.Content[i+1]
		if seq.Kind != yaml.SequenceNode {
			return nil // malformed; compose-go reports it
		}
		for _, item := range seq.Content {
			for _, n := range includeEntryPathNodes(item) {
				p := n.Value
				if !filepath.IsAbs(p) {
					p = filepath.Join(base, p)
				}
				if _, err := readCappedFile(p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return nil
}

// resolveIncludeEntry processes one `include:` list entry in place: for
// every path it names (the short scalar form names one; the long
// `{path: [...]}` override form can name several), it attempts to read,
// parse, and — unless the entry sets its own `project_directory` —
// recursively strip the target via resolveIncludeTarget. ok is false when
// ANY of the entry's paths could not be resolved, at any depth — the
// caller drops the whole entry in that case, the same way it would for a
// single unreadable top-level include. changed reports whether a path was
// rewritten in place to point at a staged temp copy (because something
// beneath it needed stripping) even though the entry itself survives.
func resolveIncludeEntry(item *yaml.Node, base string, chain map[string]bool, warn io.Writer, staged *[]string) (ok, changed bool) {
	pathNodes := includeEntryPathNodes(item)
	if len(pathNodes) == 0 {
		return true, false // can't tell the entry's shape; leave it for compose-go to validate normally
	}
	recurse := !includeEntryHasProjectDirectory(item)
	for _, n := range pathNodes {
		p := n.Value
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		} else {
			p = filepath.Clean(p)
		}
		resolved, resolvedOK := resolveIncludeTarget(p, chain, warn, staged, recurse)
		if !resolvedOK {
			return false, false
		}
		if resolved != p {
			n.Value = resolved // always an absolute staged path; a valid include target as-is
			changed = true
		}
	}
	return true, changed
}

// includeEntryHasProjectDirectory reports whether a long-form `include:`
// entry sets its own `project_directory`. compose-go honors this key (it
// is in Task 6's supported-include-keys allow-list, not warned about) to
// change where the included file's *own* relative paths — including its
// own nested `include:` targets — resolve from. This package has no way
// to compute that override without reimplementing compose-go's own
// resolution (ApplyInclude's r.ProjectDirectory handling), so an entry
// that sets it opts its target out of the recursive tolerance below: see
// resolveIncludeTarget's recurse parameter.
//
// Goes through mappingEntries (internal/compose/warn.go), the same helper
// WarnUnsupportedKeys uses, rather than a raw pairwise scan of
// item.Content: that folds a `<<` merge key and resolves aliases (with the
// same depth cap), so a `project_directory` supplied through a merge key is
// detected exactly like a literal one — a hand-rolled scan over
// item.Content only ever sees literal keys.
func includeEntryHasProjectDirectory(item *yaml.Node) bool {
	for _, e := range mappingEntries(item) {
		if e.key == "project_directory" && e.value != nil && e.value.Value != "" {
			return true
		}
	}
	return false
}

// resolveIncludeTarget attempts to read and parse p (one include entry's
// already-resolved absolute target path), and — when recurse is true —
// recursively strip p's own `include:` list the same way. ok=false covers
// every failure mode stripUnreadableIncludes documents as tolerated:
// unopenable, oversized, unparseable, or a cycle back to a file already
// being processed in this chain. On success it returns the path
// compose-go should actually read in p's place: p itself when nothing
// beneath it needed rewriting (or recursion was skipped), or a new staged
// temp file when a nested include of p's own got stripped.
//
// recurse is false exactly when the include entry that names p set its
// own `project_directory` (see includeEntryHasProjectDirectory): that
// key changes where p's *own* relative include paths resolve from —
// compose-go honors it, this package cannot compute its effect without
// reimplementing compose-go's own resolution — so looking inside p's
// include: list here would use the wrong base directory and risk
// silently DROPPING a nested include compose-go itself would have
// resolved and loaded. That is a wrong *result*, worse than doing
// nothing, so it isn't attempted: p itself is still verified openable and
// parseable (the direct target's own path is always resolved against the
// *including* file's directory, independent of project_directory, so
// that check stays valid), but its own nested includes are left
// completely untouched. A genuinely broken nested include beneath a
// project_directory override is not tolerated by this mechanism; Load
// fails on it exactly as it did before this mechanism existed — the
// "never a wrong result, only a missed tolerance" property this package
// aims for everywhere else.
func resolveIncludeTarget(p string, chain map[string]bool, warn io.Writer, staged *[]string, recurse bool) (resolved string, ok bool) {
	if chain[p] {
		return "", false
	}
	data, err := readCappedFile(p)
	if err != nil {
		composeWarn(warn, "include %s: skipped, the services it declares are not in this reload (%v)", p, err)
		return "", false
	}
	var probe yaml.Node
	if err := yaml.Unmarshal(data, &probe); err != nil {
		composeWarn(warn, "include %s: skipped, the services it declares are not in this reload (%v)", p, err)
		return "", false
	}
	if !recurse {
		return p, true
	}
	nested := make(map[string]bool, len(chain)+1)
	for k := range chain {
		nested[k] = true
	}
	nested[p] = true

	rewritten, changed, err := stripIncludesAtLevel(p, data, nested, warn, staged)
	if err != nil {
		composeWarn(warn, "include %s: skipped, the services it declares are not in this reload (%v)", p, err)
		return "", false
	}
	if !changed {
		return p, true
	}
	stagedPath, err := writeRewrittenInclude(p, rewritten)
	if err != nil {
		composeWarn(warn, "include %s: skipped, the services it declares are not in this reload (%v)", p, err)
		return "", false
	}
	*staged = append(*staged, stagedPath)
	return stagedPath, true
}

// includeEntryPathNodes returns the scalar YAML node(s) holding the
// path(s) one `include:` list entry names — the short scalar form itself,
// or the long form's `path` key (itself a scalar or a list, compose-go's
// base+overrides form). Returning the nodes themselves, not copies of
// their values, is what lets resolveIncludeEntry rewrite a path in place
// when a nested include beneath it gets staged to a temp file. Returns
// nil when the entry's shape can't be read at all, so the caller leaves
// it untouched for compose-go to reject with its own error rather than
// silently swallowing a genuinely malformed entry.
func includeEntryPathNodes(item *yaml.Node) []*yaml.Node {
	switch item.Kind {
	case yaml.ScalarNode:
		return []*yaml.Node{item}
	case yaml.MappingNode:
		for i := 0; i+1 < len(item.Content); i += 2 {
			if item.Content[i].Value != "path" {
				continue
			}
			v := item.Content[i+1]
			switch v.Kind {
			case yaml.ScalarNode:
				return []*yaml.Node{v}
			case yaml.SequenceNode:
				var out []*yaml.Node
				for _, c := range v.Content {
					if c.Kind == yaml.ScalarNode {
						out = append(out, c)
					}
				}
				return out
			}
		}
	}
	return nil
}

// writeRewrittenInclude writes rewritten to a temp file beside the
// original (same directory, so any relative path the rewritten content
// still contains resolves exactly as it would have from the original
// file) and returns its path. The caller is responsible for removing it
// once the load that consumes it has finished.
//
// This needs the original file's directory to be writable — the bespoke
// loader needed no write access at all for this tolerance. A read-only
// project directory (or a read-only directory holding a shared included
// file) makes this fail loudly rather than silently falling back to the
// stricter behavior; not fixed here, noted for whoever picks this up
// next.
func writeRewrittenInclude(originalPath string, rewritten []byte) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(originalPath), ".urunc-compose-reload-*.yaml")
	if err != nil {
		return "", fmt.Errorf("stage rewritten compose file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(rewritten); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("stage rewritten compose file: %w", err)
	}
	return f.Name(), nil
}
