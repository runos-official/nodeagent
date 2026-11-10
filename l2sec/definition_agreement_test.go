package l2sec

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// THE GENERATED FILE HOLDS EVERY FIELD TWICE: once as a Go struct tag, and once
// inside the serialized file descriptor at the bottom of the same file. The two
// are what the Go code reads and what protobuf reflection reads, and nothing in
// the toolchain makes them agree.
//
// That matters here because this repository holds the GENERATED file and no
// .proto source: the proto is hand-synced with the control plane repository,
// with no gate between them, so an additive field can be applied to one copy of
// the definition and not the other. A struct tag without its descriptor entry
// marshals nothing, and a descriptor entry without its struct tag is invisible
// to the Go code. Either way the wire silently loses the field.
//
// This test holds the two copies to each other for every message in the file.
func TestStructTagsAndDescriptorAgree(t *testing.T) {
	fd := File_nodeward_l2sec_proto
	messages := fd.Messages()
	if messages.Len() == 0 {
		t.Fatal("the file descriptor carries no messages, so this test proves nothing")
	}

	checked := 0
	for i := 0; i < messages.Len(); i++ {
		md := messages.Get(i)
		goType := goStructFor(t, md)
		if goType == nil {
			continue
		}
		byNumber := map[protoreflect.FieldNumber]string{}
		for _, f := range protobufTaggedFields(goType) {
			if existing, ok := byNumber[f.number]; ok {
				t.Errorf("%s: the Go struct uses field number %d twice, for %s and %s", md.FullName(), f.number, existing, f.name)
			}
			byNumber[f.number] = f.name
		}

		fields := md.Fields()
		for j := 0; j < fields.Len(); j++ {
			fdesc := fields.Get(j)
			name, ok := byNumber[fdesc.Number()]
			if !ok {
				t.Errorf("%s: the descriptor declares %s on field %d, but no Go struct tag carries that number, so the Go code cannot read it",
					md.FullName(), fdesc.Name(), fdesc.Number())
				continue
			}
			if name != string(fdesc.Name()) {
				t.Errorf("%s: field %d is %q in the descriptor and %q in the Go struct tag",
					md.FullName(), fdesc.Number(), fdesc.Name(), name)
			}
			delete(byNumber, fdesc.Number())
			checked++
		}
		for number, name := range byNumber {
			t.Errorf("%s: the Go struct tag carries %q on field %d, but the descriptor does not declare it, so marshalling drops it",
				md.FullName(), name, number)
		}
	}
	if checked == 0 {
		t.Fatal("no field was checked, so this test proves nothing")
	}
	t.Logf("checked %d fields across %d messages", checked, messages.Len())
}

// The install-commands request carries this agent's version so the control plane
// can gate the runtime package holds on it. The field number is hand-synced with
// the control plane repository, so it is pinned here by number, name and type.
func TestGetInstallCommandsRequestCarriesNodeAgentVersionOnFieldTen(t *testing.T) {
	md := (&GetInstallCommandsRequest{}).ProtoReflect().Descriptor()
	fd := md.Fields().ByNumber(10)
	if fd == nil {
		t.Fatal("GetInstallCommandsRequest has no field 10, so the control plane gate reads no node agent version")
	}
	if got := string(fd.Name()); got != "nodeAgentVersion" {
		t.Errorf("field 10 is %q, want nodeAgentVersion", got)
	}
	if got := fd.Kind(); got != protoreflect.StringKind {
		t.Errorf("field 10 is a %s, want a string", got)
	}
	// The field the message already had must keep its own number.
	if fd := md.Fields().ByNumber(5); fd == nil || string(fd.Name()) != "jsonB64FileList" {
		t.Errorf("field 5 is no longer jsonB64FileList, which would break every released control plane")
	}
}

type taggedField struct {
	name   string
	number protoreflect.FieldNumber
}

// protobufTaggedFields reads the name and number out of every `protobuf:"..."`
// struct tag of a generated message type.
func protobufTaggedFields(t reflect.Type) []taggedField {
	var out []taggedField
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("protobuf")
		if tag == "" {
			continue
		}
		var tf taggedField
		for _, part := range strings.Split(tag, ",") {
			if n, err := strconv.Atoi(part); err == nil && tf.number == 0 {
				tf.number = protoreflect.FieldNumber(n)
			}
			if strings.HasPrefix(part, "name=") {
				tf.name = strings.TrimPrefix(part, "name=")
			}
		}
		if tf.name != "" && tf.number != 0 {
			out = append(out, tf)
		}
	}
	return out
}

// goStructFor returns the Go struct type generated for a message descriptor, or
// nil when the message has no generated Go type in this package.
func goStructFor(t *testing.T, md protoreflect.MessageDescriptor) reflect.Type {
	t.Helper()
	mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName())
	if err != nil || mt == nil {
		return nil
	}
	rt := reflect.TypeOf(mt.New().Interface())
	for rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt.Kind() != reflect.Struct {
		return nil
	}
	return rt
}
