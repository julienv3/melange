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
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"text/template"

	"chainguard.dev/apko/pkg/tarball"
	_ "chainguard.dev/melange/internal/sign"
	"github.com/psanford/memfs"
)

type PackageContext struct {
	Context       *Context
	Origin        *Package
	PackageName   string
	InstalledSize int64
	DataHash      string
}

func (pkg *Package) Emit(ctx *PipelineContext) error {
	fakesp := Subpackage{
		Name: pkg.Name,
	}
	return fakesp.Emit(ctx)
}

func (spkg *Subpackage) Emit(ctx *PipelineContext) error {
	pc := PackageContext{
		Context:     ctx.Context,
		Origin:      &ctx.Context.Configuration.Package,
		PackageName: spkg.Name,
	}
	return pc.EmitPackage()
}

func (pc *PackageContext) Identity() string {
	return fmt.Sprintf("%s-%s-r%d", pc.PackageName, pc.Origin.Version, pc.Origin.Epoch)
}

func (pc *PackageContext) Filename() string {
	return fmt.Sprintf("%s.apk", pc.Identity())
}

func (pc *PackageContext) WorkspaceSubdir() string {
	return filepath.Join(pc.Context.WorkspaceDir, "melange-out", pc.PackageName)
}

var controlTemplate = `
# Generated by melange.
pkgname = {{.PackageName}}
pkgver = {{.Origin.Version}}-r{{.Origin.Epoch}}
arch = x86_64
size = {{.InstalledSize}}
pkgdesc = {{.Origin.Description}}
{{- range $copyright := .Origin.Copyright }}
license = {{ $copyright.License }}
{{- end }}
{{- range $dep := .Origin.Dependencies.Runtime }}
depend = {{ $dep }}
{{- end }}
datahash = {{.DataHash}}
`

func (pc *PackageContext) GenerateControlData(w io.Writer) error {
	tmpl := template.New("control")
	return template.Must(tmpl.Parse(controlTemplate)).Execute(w, pc)
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
func (pc *PackageContext) EmitPackage() error {
	log.Printf("generating package %s", pc.Identity())

	dataTarGz, err := os.CreateTemp("", "melange-data-*.tar.gz")
	if err != nil {
		return fmt.Errorf("unable to open temporary file for writing: %w", err)
	}
	defer dataTarGz.Close()

	tarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Context.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithUseChecksums(true),
	)
	if err != nil {
		return fmt.Errorf("unable to build tarball context: %w", err)
	}

	fsys := os.DirFS(pc.WorkspaceSubdir())
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		fi, err := d.Info()
		if err != nil {
			return err
		}

		pc.InstalledSize += fi.Size()
		return nil
	}); err != nil {
		return fmt.Errorf("unable to preprocess package data: %w", err)
	}

	// TODO(kaniini): generate so:/cmd: virtuals for the filesystem
	// prepare data.tar.gz
	dataDigest := sha256.New()
	dataMW := io.MultiWriter(dataDigest, dataTarGz)
	if err := tarctx.WriteArchiveFromFS(pc.WorkspaceSubdir(), fsys, dataMW); err != nil {
		return fmt.Errorf("unable to write data tarball: %w", err)
	}

	pc.DataHash = hex.EncodeToString(dataDigest.Sum(nil))
	log.Printf("  data.tar.gz installed-size: %d", pc.InstalledSize)
	log.Printf("  data.tar.gz digest: %s", pc.DataHash)

	if _, err := dataTarGz.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind data tarball: %w", err)
	}

	// prepare control.tar.gz
	var controlBuf bytes.Buffer
	if err := pc.GenerateControlData(&controlBuf); err != nil {
		return fmt.Errorf("unable to process control template: %w", err)
	}

	controlFS := memfs.New()
	if err := controlFS.WriteFile(".PKGINFO", controlBuf.Bytes(), 0644); err != nil {
		return fmt.Errorf("unable to build control FS: %w", err)
	}

	controlTarGz, err := os.CreateTemp("", "melange-control-*.tar.gz")
	if err != nil {
		return fmt.Errorf("unable to open temporary file for writing: %w", err)
	}
	defer controlTarGz.Close()

	controlDigest := sha1.New() // nolint:gosec
	controlMW := io.MultiWriter(controlDigest, controlTarGz)
	if err := tarctx.WriteArchiveFromFS(".", controlFS, controlMW); err != nil {
		return fmt.Errorf("unable to write control tarball: %w", err)
	}

	controlHash := hex.EncodeToString(controlDigest.Sum(nil))
	log.Printf("  control.tar.gz digest: %s", controlHash)

	if _, err := controlTarGz.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind control tarball: %w", err)
	}

	// TODO(kaniini): support signing
	outFile, err := os.Create(pc.Filename())
	if err != nil {
		return fmt.Errorf("unable to create apk file: %w", err)
	}
	defer outFile.Close()

	if err := combine(outFile, controlTarGz, dataTarGz); err != nil {
		return fmt.Errorf("unable to write apk file: %w", err)
	}

	log.Printf("wrote %s", outFile.Name())

	return nil
}
