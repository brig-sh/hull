package compose

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/consts"
	"github.com/compose-spec/compose-go/v2/types"
)

// Options carries everything a compose command invocation determines.
type Options struct {
	Files       []string // -f list; empty means default discovery in WorkingDir
	ProjectName string   // sanitized -p / COMPOSE_PROJECT_NAME / dirname
	WorkingDir  string
	EnvFiles    []string // --env-file list; nil means default .env discovery
	Profiles    []string // --profile / COMPOSE_PROFILES; "*" = all
	Environ     []string // caller passes os.Environ(); injectable for tests
	// Warn receives one line per unsupported compose key encountered in the
	// main config file(s) and any file pulled in via `include:` (see
	// WarnUnsupportedKeys). A nil Warn silently drops the warnings, the same
	// as pointing it at io.Discard.
	Warn io.Writer
	// SkipUnreadableIncludes tolerates a top-level `include:` entry naming a
	// file that cannot be opened: that entry (and the services it would
	// have contributed) is dropped instead of failing the whole Load. A
	// reload of a live project sets this — see reloadProject in
	// cmd/urunc-macos and stripUnreadableIncludes's own doc comment for
	// why, and for the one-level scope limit. A command a user is waiting
	// on (config, up) never sets it: there, a missing include is the whole
	// point of the error.
	SkipUnreadableIncludes bool
}

// MaxUserFileBytes caps the user-supplied text files the loader reads: a
// compose file, an env file, or a file pulled in via `include:`. A compose
// file or an env file is a document; without a cap, pointing one at an
// endless stream like /dev/zero hangs the process while it consumes the
// machine's memory. Matches maxTextFileBytes, the limit the bespoke loader
// enforced in cmd/urunc-macos/compose.go.
const MaxUserFileBytes = 8 << 20 // 8 MiB

// checkFileSize opens path and rejects it once more than MaxUserFileBytes
// have been read. It reads MaxUserFileBytes+1 bytes through a LimitReader
// and checks the count, the same pattern cmd/urunc-macos's readTextFile
// uses, rather than trusting os.Stat: a stat-then-read has a TOCTOU gap
// where the file can grow between the check and the point something reads
// it, and here compose-go does its own read afterward. The bytes read here
// are discarded; compose-go re-reads and parses the file itself, so there's
// no reason to hold a duplicate copy of a document-sized file in memory just
// to measure it.
func checkFileSize(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(io.Discard, io.LimitReader(f, MaxUserFileBytes+1))
	if err != nil {
		return err
	}
	if n > MaxUserFileBytes {
		return fmt.Errorf("%s is larger than the %d MiB limit for a text file",
			path, MaxUserFileBytes>>20)
	}
	return nil
}

// readCappedFile reads path the same way checkFileSize validates it, but
// returns the bytes instead of discarding them: the compose config files
// (unlike env files) also feed WarnUnsupportedKeys, which needs the raw
// content. Reading once and reusing it for both the cap check and the warn
// walk avoids opening every compose file twice.
func readCappedFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxUserFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxUserFileBytes {
		return nil, fmt.Errorf("%s is larger than the %d MiB limit for a text file",
			path, MaxUserFileBytes>>20)
	}
	return data, nil
}

// defaultEnvFile mirrors compose-go's own default .env discovery
// (cli.WithEnvFiles, called with no files): a .env beside workingDir, unless
// COMPOSE_DISABLE_ENV_FILE opts out. Load needs to know this path itself so
// it can cap the file before compose-go reads it; a malformed value for the
// opt-out variable is not this function's error to raise, since compose-go
// performs the authoritative parse when it builds project options, so it is
// treated here as "not disabled" and left for compose-go to report.
func defaultEnvFile(workingDir string) string {
	if disabled, err := strconv.ParseBool(os.Getenv(consts.ComposeDisableDefaultEnvFile)); err == nil && disabled {
		return ""
	}
	def := filepath.Join(workingDir, ".env")
	if info, err := os.Stat(def); err == nil && !info.IsDir() {
		return def
	}
	return ""
}

// Load parses, validates, interpolates and merges the project the way
// docker compose does, then returns the typed model.
func Load(ctx context.Context, opts Options) (*types.Project, error) {
	// opts.EnvFiles (and its default-.env fallback below) are paths we
	// control before compose-go ever sees them, so cap them up front: reject
	// an oversized file before spending any work parsing it. The main
	// compose file(s) are capped further down, once cli.NewProjectOptions
	// has resolved opts.Files against default discovery: when opts.Files is
	// empty, the actual path to check isn't known until then.
	envFiles := opts.EnvFiles
	if len(envFiles) == 0 {
		// The bespoke loader capped the default-discovered .env too
		// (cmd/urunc-macos/compose.go's parseComposeFile falls back to
		// readTextFile(srcDir/.env)); keep that hardening even though
		// compose-go, not us, ends up reading this particular path.
		if def := defaultEnvFile(opts.WorkingDir); def != "" {
			envFiles = []string{def}
		}
	}
	for _, f := range envFiles {
		if err := checkFileSize(f); err != nil {
			return nil, err
		}
	}

	fns := []cli.ProjectOptionsFn{
		cli.WithWorkingDirectory(opts.WorkingDir),
		cli.WithName(opts.ProjectName),
		// Interpolation env: caller's environ, then --env-file / default .env
		// (docker precedence: os env beats dotenv).
		cli.WithEnv(opts.Environ),
	}
	// WithEnvFiles must run unconditionally: called with no files it performs
	// default .env discovery in WorkingDir (see compose-go cli.WithEnvFiles),
	// which is what makes the "nil EnvFiles = default .env discovery" contract
	// in Options work. Skipping the call when opts.EnvFiles is empty would
	// leave EnvFiles unset and WithDotEnv would have nothing to read.
	fns = append(fns, cli.WithEnvFiles(opts.EnvFiles...))
	fns = append(fns, cli.WithDotEnv)
	if len(opts.Profiles) > 0 {
		fns = append(fns, cli.WithProfiles(opts.Profiles))
	}
	fns = append(fns,
		cli.WithDefaultConfigPath, // honors compose.yaml/docker-compose.yml discovery when Files empty
		cli.WithResolvedPaths(true),
	)
	po, err := cli.NewProjectOptions(opts.Files, fns...)
	if err != nil {
		return nil, err
	}

	// po.ConfigPaths is opts.Files verbatim when opts.Files was given, and
	// otherwise whatever cli.WithDefaultConfigPath found by walking up from
	// WorkingDir (see compose-go cli/options.go) - either way, it's the
	// actual set of main compose files about to be read, resolved without
	// having read any of their content yet. Capping opts.Files directly
	// would miss the empty case entirely: "Files empty means default
	// discovery" is the documented contract, and a discovered file is a
	// user-supplied file same as an explicit -f one.
	//
	// The same read also feeds the unsupported-key warn walk: a file named
	// directly (as opposed to one pulled in via `include:`) warns with no
	// origin prefix, exactly as the bespoke loader's top-level
	// parseComposeFile call did.
	// tempIncludeFiles collects any rewritten-and-staged copies
	// stripUnreadableIncludes produced below, cleaned up once this Load call
	// is done with them (LoadProject has to read them first).
	var tempIncludeFiles []string
	defer func() {
		for _, f := range tempIncludeFiles {
			_ = os.Remove(f)
		}
	}()

	rewrittenPaths := map[string]string{} // original main file path -> staged rewritten copy
	for _, f := range po.ConfigPaths {
		data, err := readCappedFile(f)
		if err != nil {
			return nil, err
		}
		if opts.Warn != nil {
			WarnUnsupportedKeys(opts.Warn, data, "")
		}
		if opts.SkipUnreadableIncludes {
			rewritten, changed, staged, err := stripUnreadableIncludes(f, data, opts.Warn)
			if err != nil {
				return nil, err
			}
			tempIncludeFiles = append(tempIncludeFiles, staged...)
			if changed {
				top, err := writeRewrittenInclude(f, rewritten)
				if err != nil {
					return nil, err
				}
				tempIncludeFiles = append(tempIncludeFiles, top)
				rewrittenPaths[f] = top
			}
		} else if err := capTopLevelIncludes(f, data); err != nil {
			// stripUnreadableIncludes (above) already pre-caps every
			// top-level include target as part of its own read-and-parse
			// walk, ahead of the same compose-go read this guards against;
			// this branch is only reached when that tolerant path isn't
			// running, so the cap has to be enforced here instead, still
			// before po.LoadProject ever sees these files. See
			// capTopLevelIncludes's own doc comment for why this can't wait
			// until after LoadProject returns.
			return nil, err
		}
	}
	if len(rewrittenPaths) > 0 {
		// At least one main file had an unreadable include stripped: reload
		// cli.ProjectOptions against the staged copies instead of the
		// originals, so compose-go never attempts the read that would have
		// failed the whole project. po.ConfigPaths ordering is preserved;
		// only the entries that changed are substituted.
		substituted := make([]string, len(po.ConfigPaths))
		for i, f := range po.ConfigPaths {
			if staged, ok := rewrittenPaths[f]; ok {
				substituted[i] = staged
			} else {
				substituted[i] = f
			}
		}
		po, err = cli.NewProjectOptions(substituted, fns...)
		if err != nil {
			return nil, err
		}
	}

	// Every included or extends-target file still has to be read again here
	// regardless of any cap already enforced on it, because this is also the
	// only place that knows each file's own name for the unsupported-key
	// warn-walk below (WarnUnsupportedKeys needs the raw content, not just a
	// pass/fail). That second read re-applies the size cap as a backstop —
	// for a top-level `include:` entry the cap already ran before compose-go
	// ever touched the file (capTopLevelIncludes above, when
	// Options.SkipUnreadableIncludes is unset, or stripUnreadableIncludes's
	// own pre-read when it is), so this backstop is redundant-but-harmless
	// there. For anything one level deeper — a nested include, or any
	// `extends: {file: ...}` target regardless of depth — this backstop is
	// the ONLY cap that ever runs on it, and by the time it runs here
	// compose-go's own unbounded read inside po.LoadProject has already
	// completed: too late to bound that parse, still early enough that
	// nothing acts on an oversized file's content afterward. That gap is
	// real and stays documented (see docs/adr/0009), not closed by this
	// function.
	//
	// compose-go's *types.Project carries no field listing either kind of
	// referenced file (types.Project.IncludeReferences is not currently
	// populated - see the commented-out assertion in compose-go's
	// loader/loader_test.go, "TODO(ndeloof) restore support for include
	// tracking"; extends has no equivalent field at all). The only
	// observation point for both is the telemetry Listener compose-go
	// invokes once per `include:` entry and once per `extends:` use, which
	// hand back the declared (possibly relative) path(s); record what each
	// reports and stat those files once loading finishes. compose-go only
	// fires either listener for a declaration in a file passed to Load
	// directly - one inside an included file, or inside an extends target
	// file, fires no event (compose-go clears Listeners for that nested
	// load - see loader/include.go's ApplyInclude and loader/extends.go's
	// applyServiceExtends, both explicitly), so a file reachable only
	// through a chain of includes, or a chain of extends, is not capped or
	// warn-walked by this check. That matches this package's existing
	// "one level" scope for includes; extends gets the same scope, not a
	// deeper one.
	//
	// The include event's metadata carries its own "workingdir" (the
	// declaring file's directory, since an include's path is relative to
	// wherever the `include:` list itself sits); extends carries no such
	// key at all (compose-go's own extends_test.go only ever counts the
	// event, never resolves its metadata) - but because this listener never
	// survives past a file's own top-level extends (any extends nested
	// inside an already-included or already-extended file clears
	// Listeners first, as above), an extends event we DO see always comes
	// from opts.WorkingDir itself: exactly the directory compose-go's own
	// localResourceLoader resolves a top-level extends.file against
	// (loader.go's Load call seeds that loader with configDetails.WorkingDir,
	// which is this function's opts.WorkingDir).
	var referencedFiles []string
	po.WithListeners(func(event string, metadata map[string]any) {
		switch event {
		case "include":
			workingDir, _ := metadata["workingdir"].(string)
			paths, _ := metadata["path"].(types.StringList)
			for _, p := range paths {
				if !filepath.IsAbs(p) {
					p = filepath.Join(workingDir, p)
				}
				referencedFiles = append(referencedFiles, p)
			}
		case "extends":
			// The short scalar form ('extends: SERVICE') reports metadata
			// with no "file" key at all - a same-file extends, already
			// covered by this file's own top-level warn-walk above, so
			// there is nothing more to read.
			file, ok := metadata["file"].(string)
			if !ok || file == "" {
				return
			}
			if !filepath.IsAbs(file) {
				file = filepath.Join(opts.WorkingDir, file)
			}
			referencedFiles = append(referencedFiles, file)
		}
	})

	// LoadProject already merges env_file: content into each service's
	// Environment: loader.ModelToProject (compose-go v2.14.0,
	// loader/loader.go:658-659) calls project.WithServicesEnvironmentResolved
	// unconditionally unless loader.Options.SkipResolveEnvironment is true,
	// which only happens via the explicit cli.WithoutEnvironmentResolution
	// option (cli/options.go:379-384) - not used here. So no separate
	// resolution call belongs in this function; adding
	// cli.WithoutEnvironmentResolution to fns above would silently break
	// env_file support. See TestLoadEnvFileMergesIntoEnvironment and
	// TestLoadEnvironmentOverridesEnvFile in load_test.go, which exercise
	// this against the real pipeline (including the docker precedence rule
	// that environment: beats env_file: on key collision) rather than
	// asserting it from reading source.
	project, err := po.LoadProject(ctx)
	if err != nil {
		return nil, err
	}
	// Included and extends-target files name themselves in their warnings
	// (origin = the resolved path): a warning about a key the dotted path
	// alone would send the reader looking for it in the file they named on
	// the command line, same rule the bespoke loader's parseComposeFile
	// applied.
	for _, f := range referencedFiles {
		data, err := readCappedFile(f)
		if err != nil {
			return nil, err
		}
		if opts.Warn != nil {
			WarnUnsupportedKeys(opts.Warn, data, f)
		}
	}
	return project, nil
}
