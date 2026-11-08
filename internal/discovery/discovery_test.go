package discovery

import "testing"

func TestNewStatic(t *testing.T) {
	s := NewStatic([]string{"http://z:1/varmq", "http://a:1/varmq", "http://m:1/varmq"})
	defer s.Close()
	got := s.Endpoints()
	want := []string{"http://a:1/varmq", "http://m:1/varmq", "http://z:1/varmq"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("static Close: %v", err)
	}
}
