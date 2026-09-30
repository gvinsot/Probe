package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFolderListsOfficeDocuments(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) string {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	doc := write(filepath.Join("Legal", "contract.docx"), "doc")
	write("~$contract.docx", "lock")
	write("notes.txt", "ignored")
	write(filepath.Join(".hidden", "a.xlsx"), "ignored")
	write("._deck.pptx", "resource fork")

	f := NewFolder(root)
	var got []Entry
	if err := f.List(context.Background(), func(e Entry) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != doc || got[0].Name != "contract.docx" || got[0].Folder != filepath.Join(filepath.Base(root), "Legal") {
		t.Fatalf("entries: %+v", got)
	}
	if data, err := f.Read(context.Background(), got[0], 10); err != nil || string(data) != "doc" {
		t.Fatalf("read: %q %v", data, err)
	}
	if _, err := f.Read(context.Background(), got[0], 2); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("read over the limit = %v, want ErrTooLarge", err)
	}
	if e, err := f.Stat(context.Background(), doc); err != nil || e.Size != 3 {
		t.Fatalf("stat: %+v %v", e, err)
	}
}

// The keys come from the saved state: nothing outside the folder is read.
func TestFolderStaysInsideItsRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "watched")
	os.MkdirAll(root, 0o700)
	outside := filepath.Join(parent, "secret.docx")
	os.WriteFile(outside, []byte("secret"), 0o600)
	f := NewFolder(root)
	if _, err := f.Stat(context.Background(), outside); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat outside the root = %v", err)
	}
	if _, err := f.Read(context.Background(), Entry{Key: filepath.Join(root, "..", "secret.docx")}, 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read outside the root = %v", err)
	}
	if _, err := f.Stat(context.Background(), filepath.Join(root, "gone.docx")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat of a deleted file = %v", err)
	}
}

func TestUnavailableFolderFailsTheListing(t *testing.T) {
	f := NewFolder(filepath.Join(t.TempDir(), "unplugged"))
	if err := f.List(context.Background(), func(Entry) {}); err == nil {
		t.Fatal("listing a missing folder succeeded")
	}
}
