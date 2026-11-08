package discovery

import (
	"reflect"
	"testing"
)

func TestNewStaticDoesNotAliasInput(t *testing.T) {
	in := []string{"http://b", "http://a"}
	src := NewStatic(in)

	in[0] = "http://mutated"
	in[1] = "http://mutated2"

	want := []string{"http://a", "http://b"}
	if got := src.Endpoints(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Endpoints() = %v, want %v after mutating input", got, want)
	}
}

func TestNewStaticReturnedSliceIsACopy(t *testing.T) {
	src := NewStatic([]string{"http://a", "http://b"})

	first := src.Endpoints()
	first[0] = "http://changed"

	want := []string{"http://a", "http://b"}
	if got := src.Endpoints(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Endpoints() = %v, want %v after mutating a returned slice", got, want)
	}
}

func TestNewStaticEmptyAndClose(t *testing.T) {
	src := NewStatic(nil)
	if got := src.Endpoints(); len(got) != 0 {
		t.Fatalf("Endpoints() = %v, want empty", got)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}
}

func TestK8sSourceCloseTwiceDoesNotPanic(t *testing.T) {
	k := &k8sSource{stopCh: make(chan struct{})}
	if err := k.Close(); err != nil {
		t.Fatalf("first Close() = %v", err)
	}
	if err := k.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	select {
	case <-k.stopCh:
	default:
		t.Fatalf("stopCh should be closed after Close()")
	}
}
