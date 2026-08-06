// Package grpcx is a gRPC transport that plugs into the plan exactly like httpx: Make returns a
// bound-row → Operator factory, and each response is emitted as an agnostic doc record, so the SAME
// egress transforms (projection, normalization) apply and equivalent queries yield equivalent data.
//
// All (de)serialization is dynamic, driven ONLY by proto descriptors via fullstorydev/grpcurl — no
// generated types, no per-schema structs. Drop more .proto files under proto/ and they are compiled
// in; serde just works. (The future state — documents rich in proto definitions — replaces the
// embedded files with fetched descriptors behind the same Descriptors seam.)
package grpcx

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/fullstorydev/grpcurl"
	"github.com/jhump/protoreflect/desc/protoparse"
)

//go:embed proto
var protoFS embed.FS

// ProtoFS returns the embedded proto tree (rooted at "proto/"), for test tooling and descriptor
// inspection — e.g. standing up a dynamic server from the same definitions the client uses.
func ProtoFS() fs.FS { return protoFS }

// Descriptors are the compiled proto descriptors that drive all dynamic serde.
type Descriptors struct{ src grpcurl.DescriptorSource }

// Load compiles every embedded .proto (rooted at proto/) into a grpcurl DescriptorSource.
func Load() (*Descriptors, error) {
	var files []string
	if err := fs.WalkDir(protoFS, "proto", func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !de.IsDir() && strings.HasSuffix(p, ".proto") {
			files = append(files, strings.TrimPrefix(p, "proto/"))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("grpcx: no embedded .proto files")
	}
	parser := protoparse.Parser{
		ImportPaths: []string{"."},
		Accessor:    func(name string) (io.ReadCloser, error) { return protoFS.Open("proto/" + name) },
	}
	fds, err := parser.ParseFiles(files...)
	if err != nil {
		return nil, fmt.Errorf("grpcx: parse protos: %w", err)
	}
	src, err := grpcurl.DescriptorSourceFromFileDescriptors(fds...)
	if err != nil {
		return nil, fmt.Errorf("grpcx: descriptor source: %w", err)
	}
	return &Descriptors{src: src}, nil
}
