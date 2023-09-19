// Copyright 2022 Chainguard, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"text/template"

	"github.com/chainguard-dev/go-pkgconfig"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/pgzip"

	"chainguard.dev/melange/pkg/config"

	"chainguard.dev/apko/pkg/log"
	"github.com/chainguard-dev/go-apk/pkg/tarball"
	"github.com/psanford/memfs"
	"go.opentelemetry.io/otel"
)

// pgzip's default is GOMAXPROCS(0)
//
// This is fine for single builds, but we will starve CPU for larger builds.
// 8 is our max because modern laptops tend to have ~8 performance cores, and
// large CI machines tend to have ~64 cores.
//
// This gives us near 100% utility on workstations, allows us to do ~8
// concurrent builds on giant machines, and uses only 1 core on tiny machines.
var pgzipThreads = min(runtime.GOMAXPROCS(0), 8)

func min(l, r int) int {
	if l < r {
		return l
	}

	return r
}

type PackageContext struct {
	Package *config.Package
}

func NewPackageContext(pkg *config.Package) (*PackageContext, error) {
	return &PackageContext{
		Package: pkg,
	}, nil
}

type SubpackageContext struct {
	Subpackage *config.Subpackage
}

// Create a new subpackage context
func NewSubpackageContext(pkg *config.Subpackage) (*SubpackageContext, error) {
	return &SubpackageContext{
		Subpackage: pkg,
	}, nil
}

type PackageBuild struct {
	Build         *Build
	Origin        *PackageContext
	PackageName   string
	OriginName    string
	InstalledSize int64
	DataHash      string
	OutDir        string
	Logger        log.Logger
	Dependencies  config.Dependencies
	Arch          string
	Options       config.PackageOption
	Scriptlets    config.Scriptlets
	Description   string
	URL           string
	Commit        string
}

func (pkg *PackageContext) Emit(ctx context.Context, pb *PipelineBuild) error {
	ctx, span := otel.Tracer("melange").Start(ctx, "Emit")
	defer span.End()

	fakesp := config.Subpackage{
		Name:         pkg.Package.Name,
		Dependencies: pkg.Package.Dependencies,
		Options:      pkg.Package.Options,
		Scriptlets:   pkg.Package.Scriptlets,
		Description:  pkg.Package.Description,
		URL:          pkg.Package.URL,
		Commit:       pkg.Package.Commit,
	}
	fakespctx := SubpackageContext{
		Subpackage: &fakesp,
	}
	return fakespctx.Emit(ctx, pb)
}

func (spkg *SubpackageContext) Emit(ctx context.Context, pb *PipelineBuild) error {
	pkgctx, err := NewPackageContext(&pb.Build.Configuration.Package)
	if err != nil {
		return err
	}

	pc := PackageBuild{
		Build:        pb.Build,
		Origin:       pkgctx,
		PackageName:  spkg.Subpackage.Name,
		OriginName:   spkg.Subpackage.Name,
		OutDir:       filepath.Join(pb.Build.OutDir, pb.Build.Arch.ToAPK()),
		Logger:       pb.Build.Logger,
		Dependencies: spkg.Subpackage.Dependencies,
		Arch:         pb.Build.Arch.ToAPK(),
		Options:      spkg.Subpackage.Options,
		Scriptlets:   spkg.Subpackage.Scriptlets,
		Description:  spkg.Subpackage.Description,
		URL:          spkg.Subpackage.URL,
		Commit:       spkg.Subpackage.Commit,
	}

	if !pb.Build.StripOriginName {
		pc.OriginName = pc.Origin.Package.Name
	}

	return pc.EmitPackage(ctx)
}

// AppendBuildLog will create or append a list of packages that were built by melange build
func (pc *PackageBuild) AppendBuildLog(dir string) error {
	if !pc.Build.CreateBuildLog {
		return nil
	}

	f, err := os.OpenFile(filepath.Join(dir, "packages.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	// separate with pipe so it is easy to parse
	_, err = f.WriteString(fmt.Sprintf("%s|%s|%s|%s-r%d\n", pc.Arch, pc.OriginName, pc.PackageName, pc.Origin.Package.Version, pc.Origin.Package.Epoch))
	return err
}

func (pc *PackageBuild) Identity() string {
	return fmt.Sprintf("%s-%s-r%d", pc.PackageName, pc.Origin.Package.Version, pc.Origin.Package.Epoch)
}

func (pc *PackageBuild) Filename() string {
	return fmt.Sprintf("%s/%s.apk", pc.OutDir, pc.Identity())
}

func (pc *PackageBuild) WorkspaceSubdir() string {
	return filepath.Join(pc.Build.WorkspaceDir, "melange-out", pc.PackageName)
}

var controlTemplate = `# Generated by melange.
pkgname = {{.PackageName}}
pkgver = {{.Origin.Package.Version}}-r{{.Origin.Package.Epoch}}
arch = {{.Arch}}
size = {{.InstalledSize}}
origin = {{.OriginName}}
pkgdesc = {{.Description}}
url = {{.URL}}
commit = {{.Commit}}
{{- if ne .Build.SourceDateEpoch.Unix 0 }}
builddate = {{ .Build.SourceDateEpoch.Unix }}
{{- end}}
{{- range $copyright := .Origin.Package.Copyright }}
license = {{ $copyright.License }}
{{- end }}
{{- range $dep := .Dependencies.Runtime }}
depend = {{ $dep }}
{{- end }}
{{- range $dep := .Dependencies.Provides }}
provides = {{ $dep }}
{{- end }}
{{- range $dep := .Dependencies.Replaces }}
replaces = {{ $dep }}
{{- end }}
{{- if .Dependencies.ProviderPriority }}
provider_priority = {{ .Dependencies.ProviderPriority }}
{{- end }}
{{- if .Scriptlets.Trigger.Paths }}
triggers = {{ range $item := .Scriptlets.Trigger.Paths }}{{ $item }} {{ end }}
{{- end }}
datahash = {{.DataHash}}
`

func (pc *PackageBuild) GenerateControlData(w io.Writer) error {
	tmpl := template.New("control")
	return template.Must(tmpl.Parse(controlTemplate)).Execute(w, pc)
}

func (pc *PackageBuild) generateControlSection(ctx context.Context) ([]byte, error) {
	tarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Build.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithSkipClose(true),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to build tarball context: %w", err)
	}

	var controlBuf bytes.Buffer
	if err := pc.GenerateControlData(&controlBuf); err != nil {
		return nil, fmt.Errorf("unable to process control template: %w", err)
	}

	fsys := memfs.New()
	if err := fsys.WriteFile(".PKGINFO", controlBuf.Bytes(), 0644); err != nil {
		return nil, fmt.Errorf("unable to build control FS: %w", err)
	}

	if pc.Scriptlets.Trigger.Script != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".trigger", []byte(pc.Scriptlets.Trigger.Script), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PreInstall != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".pre-install", []byte(pc.Scriptlets.PreInstall), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PostInstall != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".post-install", []byte(pc.Scriptlets.PostInstall), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PreDeinstall != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".pre-deinstall", []byte(pc.Scriptlets.PreDeinstall), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PostDeinstall != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".post-deinstall", []byte(pc.Scriptlets.PostDeinstall), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PreUpgrade != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".pre-upgrade", []byte(pc.Scriptlets.PreUpgrade), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PostUpgrade != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".post-upgrade", []byte(pc.Scriptlets.PostUpgrade), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)

	if err := tarctx.WriteTar(ctx, zw, fsys); err != nil {
		return nil, fmt.Errorf("unable to write control tarball: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("flushing control section gzip: %w", err)
	}

	return buf.Bytes(), nil
}

func (pc *PackageBuild) SignatureName() string {
	return fmt.Sprintf(".SIGN.RSA.%s.pub", filepath.Base(pc.Build.SigningKey))
}

type DependencyGenerator func(*PackageBuild, *config.Dependencies) error

func dedup(in []string) []string {
	sort.Strings(in)
	out := make([]string, 0, len(in))

	var prev string
	for _, cur := range in {
		if cur == prev {
			continue
		}
		out = append(out, cur)
		prev = cur
	}

	return out
}

func allowedPrefix(path string, prefixes []string) bool {
	for _, pfx := range prefixes {
		if strings.HasPrefix(path, pfx) {
			return true
		}
	}

	return false
}

var cmdPrefixes = []string{"bin", "sbin", "usr/bin", "usr/sbin"}

func generateCmdProviders(pc *PackageBuild, generated *config.Dependencies) error {
	if pc.Options.NoCommands {
		return nil
	}

	pc.Logger.Printf("scanning for commands...")

	fsys := readlinkFS(pc.WorkspaceSubdir())
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		mode := fi.Mode()
		if !mode.IsRegular() {
			return nil
		}

		if mode.Perm()&0555 == 0555 {
			if allowedPrefix(path, cmdPrefixes) {
				basename := filepath.Base(path)
				generated.Provides = append(generated.Provides, fmt.Sprintf("cmd:%s=%s-r%d", basename, pc.Origin.Package.Version, pc.Origin.Package.Epoch))
			}
		}

		return nil
	}); err != nil {
		return err
	}

	return nil
}

// findInterpreter looks for the PT_INTERP header and extracts the interpreter so that it
// may be used as a dependency.
func findInterpreter(bin *elf.File) (string, error) {
	for _, prog := range bin.Progs {
		if prog.Type != elf.PT_INTERP {
			continue
		}

		reader := prog.Open()
		interpBuf, err := io.ReadAll(reader)
		if err != nil {
			return "", err
		}

		interpBuf = bytes.Trim(interpBuf, "\x00")
		return string(interpBuf), nil
	}

	return "", nil
}

// dereferenceCrossPackageSymlink attempts to dereference a symlink across multiple package
// directories.
func (pc *PackageBuild) dereferenceCrossPackageSymlink(path string) (string, error) {
	libDirs := []string{"lib", "usr/lib", "lib64", "usr/lib64"}
	targetPackageNames := []string{pc.PackageName, pc.Build.Configuration.Package.Name}
	realPath, err := os.Readlink(filepath.Join(pc.WorkspaceSubdir(), path))
	if err != nil {
		return "", err
	}

	realPath = filepath.Base(realPath)

	for _, subPkg := range pc.Build.Configuration.Subpackages {
		targetPackageNames = append(targetPackageNames, subPkg.Name)
	}

	for _, pkgName := range targetPackageNames {
		basePath := filepath.Join(pc.Build.WorkspaceDir, "melange-out", pkgName)

		for _, libDir := range libDirs {
			testPath := filepath.Join(basePath, libDir, realPath)

			if _, err := os.Stat(testPath); err == nil {
				return testPath, nil
			}
		}
	}

	return "", nil
}

func generateSharedObjectNameDeps(pc *PackageBuild, generated *config.Dependencies) error {
	pc.Logger.Printf("scanning for shared object dependencies...")

	depends := map[string][]string{}

	fsys := readlinkFS(pc.WorkspaceSubdir())
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		mode := fi.Mode()

		// If it is a symlink, lets check and see if it is a library SONAME.
		if mode.Type()&fs.ModeSymlink == fs.ModeSymlink {
			if !strings.Contains(path, ".so") {
				return nil
			}

			realPath, err := pc.dereferenceCrossPackageSymlink(path)
			if err != nil {
				return nil
			}

			if realPath != "" {
				ef, err := elf.Open(realPath)
				if err != nil {
					return nil
				}
				defer ef.Close()

				sonames, err := ef.DynString(elf.DT_SONAME)
				// most likely SONAME is not set on this object
				if err != nil {
					pc.Logger.Warnf("library %s lacks SONAME", path)
					return nil
				}

				for _, soname := range sonames {
					generated.Runtime = append(generated.Runtime, fmt.Sprintf("so:%s", soname))
				}
			}

			return nil
		}

		// If it is not a regular file, we are finished processing it.
		if !mode.IsRegular() {
			return nil
		}

		if mode.Perm()&0555 == 0555 {
			basename := filepath.Base(path)

			// most likely a shell script instead of an ELF, so treat any
			// error as non-fatal.
			// TODO(kaniini): use DirFS for this
			ef, err := elf.Open(filepath.Join(pc.WorkspaceSubdir(), path))
			if err != nil {
				return nil
			}
			defer ef.Close()

			interp, err := findInterpreter(ef)
			if err != nil {
				return err
			}
			if interp != "" && !pc.Options.NoDepends {
				pc.Logger.Printf("interpreter for %s => %s", basename, interp)

				// musl interpreter is a symlink back to itself, so we want to use the non-symlink name as
				// the dependency.
				interpName := fmt.Sprintf("so:%s", filepath.Base(interp))
				interpName = strings.ReplaceAll(interpName, "so:ld-musl", "so:libc.musl")
				generated.Runtime = append(generated.Runtime, interpName)
			}

			libs, err := ef.ImportedLibraries()
			if err != nil {
				pc.Logger.Warnf("WTF: ImportedLibraries() returned error: %v", err)
				return nil
			}

			if !pc.Options.NoDepends {
				for _, lib := range libs {
					if strings.Contains(lib, ".so.") {
						generated.Runtime = append(generated.Runtime, fmt.Sprintf("so:%s", lib))
						depends[lib] = append(depends[lib], path)
					}
				}
			}

			// An executable program should never have a SONAME, but apparently binaries built
			// with some versions of jlink do.  Thus, if an interpreter is set (meaning it is an
			// executable program), we do not scan the object for SONAMEs.
			//
			// Ugh: libc.so.6 has an PT_INTERP set on itself to make the `/lib/libc.so.6 --about`
			// functionality work.  So we always generate provides entries for libc.
			if !pc.Options.NoProvides && (interp == "" || strings.HasPrefix(basename, "libc")) {
				libDirs := []string{"lib", "usr/lib", "lib64", "usr/lib64"}
				if !allowedPrefix(path, libDirs) {
					return nil
				}

				sonames, err := ef.DynString(elf.DT_SONAME)
				// most likely SONAME is not set on this object
				if err != nil {
					pc.Logger.Warnf("library %s lacks SONAME", path)
					return nil
				}

				for _, soname := range sonames {
					parts := strings.Split(soname, ".so.")

					var libver string
					if len(parts) > 1 {
						libver = parts[1]
					} else {
						libver = "0"
					}

					generated.Provides = append(generated.Provides, fmt.Sprintf("so:%s=%s", soname, libver))
				}
			}
		}

		return nil
	}); err != nil {
		return err
	}

	if pc.Build.DependencyLog != "" {
		pc.Logger.Printf("writing dependency log")

		logFile, err := os.Create(fmt.Sprintf("%s.%s", pc.Build.DependencyLog, pc.Arch))
		if err != nil {
			pc.Logger.Warnf("Unable to open dependency log: %v", err)
		}
		defer logFile.Close()

		je := json.NewEncoder(logFile)
		if err := je.Encode(depends); err != nil {
			return err
		}
	}

	return nil
}

var pkgConfigVersionRegexp = regexp.MustCompile("-(alpha|beta|rc|pre)")

// TODO(kaniini): Turn this feature on once enough of Wolfi is built with provider data.
var generateRuntimePkgConfigDeps = false

// generatePkgConfigDeps generates a list of provided pkg-config package names and versions,
// as well as dependency relationships.
func generatePkgConfigDeps(pc *PackageBuild, generated *config.Dependencies) error {
	pc.Logger.Printf("scanning for pkg-config data...")

	fsys := readlinkFS(pc.WorkspaceSubdir())
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !strings.HasSuffix(path, ".pc") {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		mode := fi.Mode()

		// Sigh.  ncurses uses symlinks to alias .pc files to other .pc files.
		// Skip the symlinks for now.
		if mode.Type()&fs.ModeSymlink == fs.ModeSymlink {
			return nil
		}

		pkg, err := pkgconfig.Load(filepath.Join(pc.WorkspaceSubdir(), path))
		if err != nil {
			pc.Logger.Warnf("Unable to load .pc file (%s) using pkgconfig: %v", path, err)
			return nil
		}

		pcName := filepath.Base(path)
		pcName, _ = strings.CutSuffix(pcName, ".pc")

		apkVersion := pkgConfigVersionRegexp.ReplaceAllString(pkg.Version, "_$1")
		if !pc.Options.NoProvides {
			generated.Provides = append(generated.Provides, fmt.Sprintf("pc:%s=%s", pcName, apkVersion))
		}

		if generateRuntimePkgConfigDeps {
			// TODO(kaniini): Capture version relationships here too.  In practice, this does not matter
			// so much though for us.
			for _, dep := range pkg.Requires {
				generated.Runtime = append(generated.Runtime, fmt.Sprintf("pc:%s", dep.Identifier))
			}

			for _, dep := range pkg.RequiresPrivate {
				generated.Runtime = append(generated.Runtime, fmt.Sprintf("pc:%s", dep.Identifier))
			}

			for _, dep := range pkg.RequiresInternal {
				generated.Runtime = append(generated.Runtime, fmt.Sprintf("pc:%s", dep.Identifier))
			}
		}

		return nil
	}); err != nil {
		return err
	}

	return nil
}

// removeSelfProvidedDeps removes dependencies which are provided by the package itself.
func removeSelfProvidedDeps(runtimeDeps, providedDeps []string) []string {
	providedDepsMap := map[string]bool{}

	for _, versionedDep := range providedDeps {
		dep := strings.Split(versionedDep, "=")[0]
		providedDepsMap[dep] = true
	}

	newRuntimeDeps := []string{}
	for _, dep := range runtimeDeps {
		_, ok := providedDepsMap[dep]
		if ok {
			continue
		}

		newRuntimeDeps = append(newRuntimeDeps, dep)
	}

	return newRuntimeDeps
}

func (pc *PackageBuild) GenerateDependencies() error {
	generated := config.Dependencies{}
	generators := []DependencyGenerator{
		generateSharedObjectNameDeps,
		generateCmdProviders,
		generatePkgConfigDeps,
	}

	for _, gen := range generators {
		if err := gen(pc, &generated); err != nil {
			return err
		}
	}

	newruntime := append(pc.Dependencies.Runtime, generated.Runtime...)
	pc.Dependencies.Runtime = dedup(newruntime)

	newprovides := append(pc.Dependencies.Provides, generated.Provides...)
	pc.Dependencies.Provides = dedup(newprovides)

	pc.Dependencies.Runtime = removeSelfProvidedDeps(pc.Dependencies.Runtime, pc.Dependencies.Provides)

	pc.Dependencies.Summarize(pc.Logger)

	return nil
}

func combine(out io.Writer, inputs ...io.Reader) error {
	for _, input := range inputs {
		if _, err := io.Copy(out, input); err != nil {
			return err
		}
	}

	return nil
}

// TODO(kaniini): generate APKv3 packages
func (pc *PackageBuild) calculateInstalledSize(fsys fs.FS) error {
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		pc.InstalledSize += fi.Size()
		return nil
	}); err != nil {
		return fmt.Errorf("unable to preprocess package data: %w", err)
	}

	return nil
}

func (pc *PackageBuild) emitDataSection(ctx context.Context, fsys fs.FS, w io.WriteSeeker) error {
	tarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Build.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithUseChecksums(true),
	)
	if err != nil {
		return fmt.Errorf("unable to build tarball context: %w", err)
	}

	digest := sha256.New()
	mw := io.MultiWriter(digest, w)
	zw := pgzip.NewWriter(mw)
	if err := zw.SetConcurrency(1<<20, pgzipThreads); err != nil {
		return fmt.Errorf("tried to set pgzip concurrency to %d: %w", pgzipThreads, err)
	}

	if err := tarctx.WriteTar(ctx, zw, fsys); err != nil {
		return fmt.Errorf("unable to write data tarball: %w", err)
	}

	if err := zw.Close(); err != nil {
		return fmt.Errorf("flushing data section gzip: %w", err)
	}

	pc.DataHash = hex.EncodeToString(digest.Sum(nil))
	pc.Logger.Printf("  data.tar.gz digest: %s", pc.DataHash)

	if _, err := w.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind data tarball: %w", err)
	}

	return nil
}

func (pc *PackageBuild) wantSignature() bool {
	return pc.Build.SigningKey != ""
}

func (pc *PackageBuild) EmitPackage(ctx context.Context) error {
	err := os.MkdirAll(pc.WorkspaceSubdir(), 0o755)
	if err != nil {
		return fmt.Errorf("unable to ensure workspace exists: %w", err)
	}

	pc.Logger.Printf("generating package %s", pc.Identity())

	// filesystem for the data package
	fsys := readlinkFS(pc.WorkspaceSubdir())

	// generate so:/cmd: virtuals for the filesystem
	if err := pc.GenerateDependencies(); err != nil {
		return fmt.Errorf("unable to build final dependencies set: %w", err)
	}

	// walk the filesystem to calculate the installed-size
	if err := pc.calculateInstalledSize(fsys); err != nil {
		return err
	}

	pc.Logger.Printf("  installed-size: %d", pc.InstalledSize)

	// prepare data.tar.gz
	dataTarGz, err := os.CreateTemp("", "melange-data-*.tar.gz")
	if err != nil {
		return fmt.Errorf("unable to open temporary file for writing: %w", err)
	}
	defer dataTarGz.Close()
	defer os.Remove(dataTarGz.Name())

	if err := pc.emitDataSection(ctx, fsys, dataTarGz); err != nil {
		return err
	}

	controlSectionData, err := pc.generateControlSection(ctx)
	if err != nil {
		return err
	}

	combinedParts := []io.Reader{bytes.NewReader(controlSectionData), dataTarGz}

	if pc.wantSignature() {
		signatureData, err := EmitSignature(ctx, pc.Signer(), controlSectionData, pc.Build.SourceDateEpoch)
		if err != nil {
			return fmt.Errorf("emitting signature: %v", err)
		}

		combinedParts = append([]io.Reader{bytes.NewReader(signatureData)}, combinedParts...)
	}

	// build the final tarball
	if err := os.MkdirAll(pc.OutDir, 0755); err != nil {
		return fmt.Errorf("unable to create output directory: %w", err)
	}

	outFile, err := os.Create(pc.Filename())
	if err != nil {
		return fmt.Errorf("unable to create apk file: %w", err)
	}
	defer outFile.Close()

	if err := combine(outFile, combinedParts...); err != nil {
		return fmt.Errorf("unable to write apk file: %w", err)
	}

	pc.Logger.Printf("wrote %s", outFile.Name())

	// add the package to the build log if requested
	if err := pc.AppendBuildLog(""); err != nil {
		pc.Logger.Warnf("unable to append package log: %s", err)
	}

	return nil
}

func (pc *PackageBuild) Signer() ApkSigner {
	var signer ApkSigner
	if pc.Build.SigningKey == "" {
		signer = &FulcioApkSigner{}
	} else {
		signer = &KeyApkSigner{
			KeyFile:       pc.Build.SigningKey,
			KeyPassphrase: pc.Build.SigningPassphrase,
		}
	}
	return signer
}
