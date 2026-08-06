// Package grpctest is an httptest-style helper: it serves grpcx's embedded storage proto DYNAMICALLY
// (dynamicpb — no generated code), returning canned buckets. RegisterStorage attaches the service to
// any grpc.Server (used by the standalone cmd/grpcmock on a real port); NewStorageServer wraps it on
// an in-process bufconn (used by the transport + sdk equivalence tests). One server, no duplication.
package grpctest

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/bufbuild/protocompile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/stackql-labs/omnisdk/internal/system_g/grpcx"
)

// Bucket is a canned bucket the fake server returns.
type Bucket struct {
	Name                   string
	KMSKey                 string // encryption.default_kms_key_name ("" = none)
	PublicAccessPrevention string // iam_config.public_access_prevention
	Versioning             bool   // versioning.enabled
}

// RegisterStorage attaches an omni.storage.v1.Storage service to srv that returns buckets.
func RegisterStorage(srv *grpc.Server, buckets []Bucket) error {
	method, err := methodDesc()
	if err != nil {
		return err
	}
	resp := response(method.Output(), buckets)
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: string(method.Parent().FullName()),
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: string(method.Name()),
			Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				in := dynamicpb.NewMessage(method.Input())
				if err := dec(in); err != nil {
					return nil, err
				}
				return resp, nil
			},
		}},
	}, nil)
	return nil
}

// NewStorageServer starts an in-process server returning buckets; returns a target + dial options (a
// bufconn dialer + insecure creds) and a stop func.
func NewStorageServer(buckets []Bucket) (target string, dialOpts []grpc.DialOption, stop func(), err error) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	if err := RegisterStorage(srv, buckets); err != nil {
		return "", nil, nil, err
	}
	go func() { _ = srv.Serve(lis) }()
	dialOpts = []grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	return "passthrough:///bufnet", dialOpts, srv.Stop, nil
}

func methodDesc() (protoreflect.MethodDescriptor, error) {
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			Accessor: func(p string) (io.ReadCloser, error) { return grpcx.ProtoFS().Open("proto/" + p) },
		}),
	}
	files, err := c.Compile(context.Background(), "google/storage/v2/storage.proto")
	if err != nil {
		return nil, fmt.Errorf("grpctest: compile proto: %w", err)
	}
	return files[0].Services().Get(0).Methods().Get(0), nil
}

func response(respDesc protoreflect.MessageDescriptor, buckets []Bucket) *dynamicpb.Message {
	resp := dynamicpb.NewMessage(respDesc)
	list := resp.Mutable(respDesc.Fields().ByName("buckets")).List()
	bucketDesc := respDesc.Fields().ByName("buckets").Message()
	for _, b := range buckets {
		list.Append(protoreflect.ValueOfMessage(bucketMsg(bucketDesc, b)))
	}
	return resp
}

func bucketMsg(md protoreflect.MessageDescriptor, b Bucket) protoreflect.Message {
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("name"), protoreflect.ValueOfString("projects/_/buckets/"+b.Name))
	m.Set(md.Fields().ByName("bucket_id"), protoreflect.ValueOfString(b.Name))
	if b.KMSKey != "" {
		enc := m.Mutable(md.Fields().ByName("encryption")).Message()
		enc.Set(enc.Descriptor().Fields().ByName("default_kms_key"), protoreflect.ValueOfString(b.KMSKey))
	}
	iam := m.Mutable(md.Fields().ByName("iam_config")).Message()
	iam.Set(iam.Descriptor().Fields().ByName("public_access_prevention"), protoreflect.ValueOfString(b.PublicAccessPrevention))
	ver := m.Mutable(md.Fields().ByName("versioning")).Message()
	ver.Set(ver.Descriptor().Fields().ByName("enabled"), protoreflect.ValueOfBool(b.Versioning))
	return m
}
