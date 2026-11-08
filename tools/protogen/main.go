// Command protogen keeps l2sec/nodeward_l2sec.pb.go honest, and computes the
// bytes for a new field without protoc and without a .proto file.
//
// WHY THIS EXISTS. This repository holds the GENERATED file and no .proto
// source: the proto is hand-synced with the control plane repository, with no
// gate between the two. So an additive field means editing the generated file,
// which holds every field TWICE, once as a Go struct tag and once inside the
// serialized descriptor at the bottom of the same file. An edit that changes one
// copy and not the other is silent drift on the wire.
//
// IT DOES NOT OVERWRITE THE GENERATED FILE, and that is deliberate. The compiled
// package carries its descriptor without SourceCodeInfo, so protoc-gen-go run
// from it produces correct code with EVERY PROTO COMMENT DELETED. Those comments
// are the documentation of a wire contract shared with another repository, so a
// full regeneration would be a real loss. This program therefore reports and
// verifies, and a person applies the bytes it prints.
//
// TWO MODES.
//
//	go run ./tools/protogen           verify the committed file (what `make proto-check` runs)
//	go run ./tools/protogen -print DIR  write the reference generated file into DIR, to diff against
//
// Verify regenerates from the committed descriptor and compares, ignoring
// comments and alignment. A difference means the generated file carries
// something protoc-gen-go would not produce, which is exactly the hand-edit
// drift this program guards. It also checks that every field in addedFields is
// really in the committed descriptor.
//
// ADDING A FIELD.
//  1. Add a row to addedFields and pick the next free number; the numbers in
//     this file step by five, and a number must NEVER be reused.
//  2. Run `go run ./tools/protogen -print /tmp/ref` and diff /tmp/ref against
//     l2sec/nodeward_l2sec.pb.go. The diff shows the struct field, the getter
//     and the two descriptor changes: the new field's bytes and the message's
//     length prefix.
//  3. Apply those hunks to the committed file, keeping every comment.
//  4. Run `make proto-check` and `go test ./l2sec/`. The agreement test in
//     l2sec/definition_agreement_test.go holds the two copies to each other.
//  5. Tell the control plane repository, in NOTES-nodeward.md.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"bytes"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"

	pb "github.com/runos-official/nodeagent/l2sec"
)

// generatedFile is the file this program verifies, relative to the repository root.
const generatedFile = "l2sec/nodeward_l2sec.pb.go"

// pluginVersion is the protoc-gen-go version that generated the committed file.
// Keep it pinned: a different plugin version rewrites the whole file and buries
// the one field being added in unrelated churn.
const pluginVersion = "google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10"

// compilerVersion is the protoc version recorded in the committed file's header.
// It is only a comment in the output, and pinning it keeps the diff small.
var compilerVersion = &pluginpb.Version{
	Major: proto.Int32(6), Minor: proto.Int32(33), Patch: proto.Int32(0),
}

// addedField is one field this repository added after the control plane
// repository last handed over the proto.
type addedField struct {
	Message string
	Name    string
	Number  int32
	Type    descriptorpb.FieldDescriptorProto_Type
	Why     string
}

// addedFields is the record of what this repository added, and the list verify
// checks against the committed descriptor. Every addition stays here.
var addedFields = []addedField{
	{
		Message: "GetInstallCommandsRequest",
		Name:    "nodeAgentVersion",
		Number:  10,
		Type:    descriptorpb.FieldDescriptorProto_TYPE_STRING,
		Why:     "the control plane gates the runtime package holds on the agent version, and must read it from the request, because a node being installed may not have sent a heartbeat yet",
	},
}

func main() {
	printTo := flag.String("print", "", "write the reference generated file into this directory instead of verifying")
	root := flag.String("root", ".", "repository root")
	flag.Parse()

	fdp := protodesc.ToFileDescriptorProto(pb.File_nodeward_l2sec_proto)

	if *printTo != "" {
		for _, f := range addedFields {
			if err := applyField(fdp, f); err != nil {
				fail(err)
			}
		}
		out, err := runPlugin(fdp)
		if err != nil {
			fail(err)
		}
		path := filepath.Join(*printTo, filepath.Base(generatedFile))
		if err := os.MkdirAll(*printTo, 0o755); err != nil {
			fail(err)
		}
		if err := os.WriteFile(path, out, 0o644); err != nil {
			fail(err)
		}
		fmt.Printf("reference output written to %s (%d bytes)\n", path, len(out))
		fmt.Printf("diff it against %s; it carries no proto comments, so ignore comment-only differences\n", generatedFile)
		return
	}

	verify(*root, fdp)
}

// verify holds the committed file to two things: every declared added field is
// really in its descriptor, and the whole file is what protoc-gen-go would
// produce from that descriptor, apart from the comments a regeneration drops.
func verify(root string, fdp *descriptorpb.FileDescriptorProto) {
	var problems []string

	for _, f := range addedFields {
		switch err := fieldPresent(fdp, f); {
		case err != nil:
			problems = append(problems, err.Error())
		default:
			fmt.Printf("declared field present: %s.%s = %d (%s)\n", f.Message, f.Name, f.Number, f.Type)
		}
	}

	out, err := runPlugin(fdp)
	if err != nil {
		fail(err)
	}
	path := filepath.Join(root, generatedFile)
	have, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	if normalize(string(have)) != normalize(string(out)) {
		problems = append(problems, fmt.Sprintf(
			"%s is NOT what protoc-gen-go produces from its own descriptor, ignoring comments and alignment.\n"+
				"Run: go run ./tools/protogen -print /tmp/ref   and diff /tmp/ref against the committed file.", generatedFile))
	} else {
		fmt.Printf("%s agrees with protoc-gen-go, ignoring comments and alignment\n", generatedFile)
	}

	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "protogen:", p)
		}
		os.Exit(1)
	}
	fmt.Println("protogen: ok")
}

// normalize drops comments and collapses whitespace, so a comparison sees the
// code and the descriptor bytes and not the comments a regeneration deletes or
// the gofmt alignment a new struct field shifts.
func normalize(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(trimTrailingTagComment(line))
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		b.WriteString(strings.Join(strings.Fields(t), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// trimTrailingTagComment cuts a comment that trails a struct field's tag, which
// a regeneration also deletes. It keys on the backtick of the tag, so it cannot
// touch a descriptor line: those are double-quoted and carry no backtick.
func trimTrailingTagComment(line string) string {
	tick := strings.LastIndexByte(line, '`')
	if tick < 0 {
		return line
	}
	if i := strings.Index(line[tick:], "//"); i >= 0 {
		return line[:tick+i]
	}
	return line
}

// fieldPresent reports whether the descriptor already carries this field with
// the declared name, number and type.
func fieldPresent(fdp *descriptorpb.FileDescriptorProto, f addedField) error {
	for _, m := range fdp.MessageType {
		if m.GetName() != f.Message {
			continue
		}
		for _, existing := range m.Field {
			if existing.GetNumber() != f.Number {
				continue
			}
			if existing.GetName() != f.Name {
				return fmt.Errorf("%s field %d is %q, but %q is declared", f.Message, f.Number, existing.GetName(), f.Name)
			}
			if existing.GetType() != f.Type {
				return fmt.Errorf("%s.%s is a %s, but %s is declared", f.Message, f.Name, existing.GetType(), f.Type)
			}
			return nil
		}
		return fmt.Errorf("%s has no field %d, so %q never reaches the wire", f.Message, f.Number, f.Name)
	}
	return fmt.Errorf("message %q is not in the descriptor", f.Message)
}

// applyField adds one field to one message of the descriptor. It is idempotent
// when the field is already there and identical, and refuses a conflict rather
// than overwriting it, because a number that means two things is the drift this
// program exists to prevent.
func applyField(fdp *descriptorpb.FileDescriptorProto, f addedField) error {
	for _, m := range fdp.MessageType {
		if m.GetName() != f.Message {
			continue
		}
		for _, existing := range m.Field {
			if existing.GetNumber() == f.Number || existing.GetName() == f.Name {
				if existing.GetName() == f.Name && existing.GetNumber() == f.Number && existing.GetType() == f.Type {
					return nil
				}
				return fmt.Errorf("%s already has field %q number %d type %s, which conflicts with %q number %d type %s",
					f.Message, existing.GetName(), existing.GetNumber(), existing.GetType(), f.Name, f.Number, f.Type)
			}
		}
		m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{
			Name:     proto.String(f.Name),
			JsonName: proto.String(f.Name),
			Number:   proto.Int32(f.Number),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     f.Type.Enum(),
		})
		return nil
	}
	return fmt.Errorf("message %q is not in the descriptor", f.Message)
}

// runPlugin pipes a CodeGeneratorRequest to protoc-gen-go and returns the single
// file it generates.
func runPlugin(fdp *descriptorpb.FileDescriptorProto) ([]byte, error) {
	req := &pluginpb.CodeGeneratorRequest{
		FileToGenerate:  []string{fdp.GetName()},
		ProtoFile:       []*descriptorpb.FileDescriptorProto{fdp},
		CompilerVersion: compilerVersion,
	}
	in, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshalling the generator request: %w", err)
	}

	cmd := exec.Command("go", "run", pluginVersion)
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("running %s: %w (%s)", pluginVersion, err, stderr.String())
	}

	var res pluginpb.CodeGeneratorResponse
	if err := proto.Unmarshal(stdout.Bytes(), &res); err != nil {
		return nil, fmt.Errorf("reading the generator response: %w", err)
	}
	if res.Error != nil {
		return nil, fmt.Errorf("the plugin refused the request: %s", res.GetError())
	}
	if len(res.File) != 1 {
		return nil, fmt.Errorf("the plugin returned %d files, want 1", len(res.File))
	}
	return []byte(res.File[0].GetContent()), nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "protogen:", err)
	os.Exit(1)
}
