package compose

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// supportedTopLevelKeys is the set of top-level compose elements this runtime
// reads. Everything else — networks, secrets, configs, profiles, version,
// name, models — is dropped, and dropping a whole section of someone's file in
// silence is the worst possible UX.
var supportedTopLevelKeys = map[string]bool{
	"services": true,
	"volumes":  true,
	"include":  true,
}

// supportedIncludeKeys is the long form of an include entry. The whole set is
// honored, so anything else there is a typo or a key from a newer spec, and
// either way it warns.
var supportedIncludeKeys = map[string]bool{
	"path": true, "project_directory": true, "env_file": true,
}

// supportedServiceKeys is the subset of the compose spec this runtime
// understands; anything else is dropped, and silence would be the worst UX
// for people bringing existing files.
var supportedServiceKeys = map[string]bool{
	"image": true, "container_name": true, "command": true,
	"environment": true, "depends_on": true, "mem_limit": true,
	"cpus": true, "volumes": true, "ports": true,
	"x-hypervisor": true, "x-healthcheck-tcp": true, "env_file": true,
	"x-oneshot": true, "restart": true, "profiles": true,
	"healthcheck": true, "post_start": true, "pre_stop": true,
	// extends is resolved by compose-go during Load, before this walk ever
	// sees the raw YAML: the service it names is merged in already, so the
	// key is not ignored the way an unsupported key is, and must not warn.
	"extends": true,
}

// supportedDependsOnKeys / supportedHealthTCPKeys are the nested mappings the
// parser looks inside, so unsupported keys there are caught too.
var supportedDependsOnKeys = map[string]bool{"condition": true, "required": true}

var supportedHealthTCPKeys = map[string]bool{
	"port": true, "interval": true, "retries": true, "start_period": true,
}

var supportedHealthcheckKeys = map[string]bool{
	"test": true, "interval": true, "timeout": true, "retries": true,
	"start_period": true, "disable": true,
}

var supportedHookKeys = map[string]bool{
	"command": true, "user": true, "environment": true,
}

// topLevelKeyHints / serviceKeyHints / nestedKeyHints give an actionable
// alternative for the unsupported keys people actually write. Keys not listed
// fall back to a generic message.
var topLevelKeyHints = map[string]string{
	"networks": "every service joins one flat project network; set its range with 'compose up --subnet CIDR'",
	"secrets":  "secrets are not implemented; pass the value through the service's 'environment' instead",
	"configs":  "configs are not implemented; bind-mount the file through the service's 'volumes' list instead",
	"profiles": "the compose spec declares profiles on a service, not at the top level; move the list under the services that need it",
	"version":  "the top-level 'version' field is obsolete in the compose spec and has no effect",
	"name":     "the project name comes from -p/--project-name, COMPOSE_PROJECT_NAME, or the working directory name",
	"models":   "model definitions are not implemented",
}

var serviceKeyHints = map[string]string{
	"build":           "images are never built; push the image and point 'image' at it",
	"networks":        "every service joins the single flat project network; per-service network selection is ignored",
	"deploy":          "deploy (replicas, resource reservations, placement) is not implemented; one service is exactly one VM",
	"secrets":         "secrets are not implemented; pass the value through 'environment' instead",
	"configs":         "configs are not implemented; bind-mount the file through 'volumes' instead",
	"entrypoint":      "the image entrypoint cannot be overridden; put the whole command line in 'command'",
	"expose":          "expose has no effect; services already reach each other on every port of the project network",
	"logging":         "logging drivers are not implemented; use 'compose logs'",
	"user":            "the guest user cannot be changed; the image's user is used",
	"working_dir":     "the working directory cannot be changed; it comes from the image",
	"privileged":      "privileged has no meaning for a VM guest and is ignored",
	"tmpfs":           "tmpfs mounts are not implemented",
	"sysctls":         "sysctls are not implemented",
	"devices":         "device passthrough is not implemented",
	"cap_add":         "capabilities have no meaning for a VM guest and are ignored",
	"cap_drop":        "capabilities have no meaning for a VM guest and are ignored",
	"hostname":        "the guest hostname cannot be set; it defaults to the image's",
	"mem_reservation": "memory reservations are not implemented; 'mem_limit' sets the guest's memory",
	"cpu_shares":      "CPU shares are not implemented; 'cpus' sets whole vCPUs",
}

var nestedKeyHints = map[string]string{
	"depends_on.restart":        "restarting a dependent when its dependency restarts is not implemented",
	"x-healthcheck-tcp.timeout": "there is no per-probe timeout; size the wait with 'retries' and 'start_period'",
}

// composeWarn writes one warning line to w. Every compose front-end warning
// goes through here so the lines stay stable and greppable: exactly one line
// per problem, all of them prefixed "warning: compose: ".
func composeWarn(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, "warning: compose: "+format+"\n", args...)
}

// WarnUnsupportedKeys walks the raw YAML and emits one warning per key this
// runtime does not act on, at every level: top-level elements, service keys,
// and the nested depends_on / x-healthcheck-tcp mappings. The key is reported
// as a dotted path ("services.web.restart") so a warning is unambiguous no
// matter how deep the key sits. origin names the file when it is one an
// include pulled in, because the dotted path alone would send the reader
// looking for the key in the file they named on the command line.
func WarnUnsupportedKeys(w io.Writer, data []byte, origin string) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return
	}
	root := asMapping(&doc)
	if root == nil {
		return
	}
	prefix := ""
	if origin != "" {
		prefix = origin + ": "
	}
	seen := map[string]bool{}
	warn := func(path, hint string) {
		if seen[path] {
			return
		}
		seen[path] = true
		composeWarn(w, "%signoring unsupported key %q: %s", prefix, path, hint)
	}

	var services *yaml.Node
	for _, top := range mappingEntries(root) {
		if supportedTopLevelKeys[top.key] {
			switch top.key {
			case "services":
				services = top.value
			case "volumes":
				// Bare declarations are honored; every per-volume option is
				// not (ADR-0006 scope fence), so each warns individually.
				for _, vol := range mappingEntries(top.value) {
					for _, opt := range mappingEntries(vol.value) {
						warn("volumes."+vol.key+"."+opt.key,
							"named-volume declaration options are not supported; only the bare declaration is honored")
					}
				}
			case "include":
				// The long form's own keys are all honored, so anything else
				// inside an entry is dropped and has to say so.
				for i, entry := range top.value.Content {
					for _, opt := range mappingEntries(entry) {
						if supportedIncludeKeys[opt.key] {
							continue
						}
						warn(fmt.Sprintf("include[%d].%s", i, opt.key),
							"an include entry reads 'path', 'project_directory' and 'env_file'")
					}
				}
			}
			continue
		}
		warn(top.key, topLevelHint(top.key))
	}
	if services == nil {
		return
	}

	for _, svc := range mappingEntries(services) {
		prefix := "services." + svc.key + "."
		for _, key := range mappingEntries(svc.value) {
			if !supportedServiceKeys[key.key] {
				warn(prefix+key.key, serviceKeyHint(key.key))
				continue
			}
			switch key.key {
			case "depends_on":
				// Only the map form has nested keys; the list form is
				// a plain sequence of service names.
				for _, dep := range mappingEntries(key.value) {
					for _, sub := range mappingEntries(dep.value) {
						if supportedDependsOnKeys[sub.key] {
							continue
						}
						warn(prefix+"depends_on."+dep.key+"."+sub.key,
							nestedHint("depends_on."+sub.key,
								"not supported by urunc-macos compose; only 'condition' is read"))
					}
				}
			case "x-healthcheck-tcp":
				for _, sub := range mappingEntries(key.value) {
					if supportedHealthTCPKeys[sub.key] {
						continue
					}
					warn(prefix+"x-healthcheck-tcp."+sub.key,
						nestedHint("x-healthcheck-tcp."+sub.key,
							"not supported by urunc-macos compose; the TCP probe reads port, interval, retries and start_period"))
				}
			case "healthcheck":
				for _, sub := range mappingEntries(key.value) {
					if supportedHealthcheckKeys[sub.key] {
						continue
					}
					warn(prefix+"healthcheck."+sub.key,
						nestedHint("healthcheck."+sub.key,
							"not supported by urunc-macos compose; the exec probe reads test, interval, timeout, retries, start_period and disable"))
				}
			case "post_start", "pre_stop":
				// Hooks are a sequence of mappings; walk each entry.
				if key.value != nil && key.value.Kind == yaml.SequenceNode {
					for i, item := range key.value.Content {
						for _, sub := range mappingEntries(item) {
							if supportedHookKeys[sub.key] {
								continue
							}
							warn(fmt.Sprintf("%s%s[%d].%s", prefix, key.key, i, sub.key),
								"not supported by urunc-macos compose; hooks read command, user and environment")
						}
					}
				}
			}
		}
	}
}

// topLevelHint explains a dropped top-level element. Extension keys get their
// own wording: a "x-common" anchor block is not interpreted as configuration,
// but a "<<" reference to it still merges into the service that uses it.
func topLevelHint(key string) string {
	if h, ok := topLevelKeyHints[key]; ok {
		return h
	}
	if strings.HasPrefix(key, "x-") {
		return "top-level extension keys are not interpreted; a YAML anchor defined here still merges where it is referenced"
	}
	return "urunc-macos compose reads only the top-level 'services' key"
}

// serviceKeyHint explains a dropped service key. The x- case catches typos in
// this runtime's own extensions, which would otherwise look like they took
// effect.
func serviceKeyHint(key string) string {
	if h, ok := serviceKeyHints[key]; ok {
		return h
	}
	if strings.HasPrefix(key, "x-") {
		return "unknown extension key; urunc-macos compose defines only x-hypervisor and x-healthcheck-tcp"
	}
	return "not supported by urunc-macos compose (see docs/compose.md for the supported service keys)"
}

// nestedHint explains a dropped key inside a mapping the parser reads. The
// lookup key is the dotted path relative to the service ("depends_on.restart")
// so the same leaf name can differ per parent.
func nestedHint(lookup, fallback string) string {
	if h, ok := nestedKeyHints[lookup]; ok {
		return h
	}
	return fallback
}

// nodeEntry is one key/value pair of a YAML mapping.
type nodeEntry struct {
	key   string
	value *yaml.Node
}

// mappingEntries returns n's mapping entries sorted by key, following aliases
// and folding YAML merge keys ("<<") into the level that references them, so
// anchor-based compose files report the keys a reader would expect. A
// non-mapping (scalar, sequence, missing) yields nothing.
func mappingEntries(n *yaml.Node) []nodeEntry {
	return mappingEntriesDepth(n, 0)
}

// mergeDepth caps merge-key recursion: a self-referencing anchor would
// otherwise walk forever.
const mergeDepth = 8

func mappingEntriesDepth(n *yaml.Node, depth int) []nodeEntry {
	m := asMapping(n)
	if m == nil || depth > mergeDepth {
		return nil
	}
	var out []nodeEntry
	var merges []*yaml.Node
	seen := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		key, value := m.Content[i].Value, m.Content[i+1]
		if key == "<<" {
			merges = append(merges, value)
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, nodeEntry{key: key, value: value})
	}
	// Merged keys lose to keys written directly in the mapping.
	for _, merge := range merges {
		for _, src := range mergeSources(merge) {
			for _, e := range mappingEntriesDepth(src, depth+1) {
				if seen[e.key] {
					continue
				}
				seen[e.key] = true
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// mergeSources returns the mappings a "<<" value refers to: one mapping, or a
// sequence of them.
func mergeSources(n *yaml.Node) []*yaml.Node {
	r := resolveAlias(n)
	if r == nil {
		return nil
	}
	if r.Kind == yaml.SequenceNode {
		return r.Content
	}
	return []*yaml.Node{r}
}

// asMapping unwraps a document node and any aliases and returns the mapping
// node, or nil if the value is not a mapping.
func asMapping(n *yaml.Node) *yaml.Node {
	n = resolveAlias(n)
	if n != nil && n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = resolveAlias(n.Content[0])
	}
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode && i <= mergeDepth; i++ {
		n = n.Alias
	}
	return n
}
