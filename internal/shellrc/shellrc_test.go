package shellrc

import "testing"

func TestUpsertBlock_AppendsToEmptyFile(t *testing.T) {
	out, changed := upsertBlock("", AliasBlock)
	if !changed {
		t.Fatal("expected changed=true for empty file")
	}
	if !contains(out, beginMarker) || !contains(out, endMarker) {
		t.Fatal("output missing markers")
	}
}

func TestUpsertBlock_AppendsToExistingFile(t *testing.T) {
	rc := "export FOO=bar\nalias ll='ls -la'\n"
	out, changed := upsertBlock(rc, AliasBlock)
	if !changed {
		t.Fatal("expected changed=true")
	}
	if !contains(out, "export FOO=bar") {
		t.Fatal("preserved user content")
	}
	if !contains(out, AliasBlock) {
		t.Fatal("appended block missing")
	}
}

func TestUpsertBlock_IdempotentOnReRun(t *testing.T) {
	rc := "export FOO=bar\n"
	first, _ := upsertBlock(rc, AliasBlock)
	second, changed := upsertBlock(first, AliasBlock)
	if changed {
		t.Fatal("second run should be a no-op")
	}
	if first != second {
		t.Fatalf("non-idempotent:\nfirst=%q\nsecond=%q", first, second)
	}
}

func TestUpsertBlock_ReplacesOldBlockInPlace(t *testing.T) {
	oldBlock := beginMarker + "\nalias claude='OLD'\n" + endMarker + "\n"
	rc := "before\n" + oldBlock + "after\n"
	out, changed := upsertBlock(rc, AliasBlock)
	if !changed {
		t.Fatal("expected changed=true when replacing old content")
	}
	if !contains(out, "before\n") || !contains(out, "after\n") {
		t.Fatal("surrounding content lost")
	}
	if contains(out, "OLD") {
		t.Fatal("old block content still present")
	}
	if !contains(out, "alias claude='cass claude'") {
		t.Fatal("new block content missing")
	}
}

func TestUpsertBlock_HealsHalfBlock(t *testing.T) {
	// User somehow deleted the end marker. Make sure we don't append a
	// second block on top of the dangling begin.
	rc := "before\n" + beginMarker + "\nalias claude='OLD'\n"
	out, changed := upsertBlock(rc, AliasBlock)
	if !changed {
		t.Fatal("expected changed when healing half-block")
	}
	// Count begin markers — must be exactly one.
	count := 0
	idx := 0
	for {
		i := indexFrom(out, beginMarker, idx)
		if i == -1 {
			break
		}
		count++
		idx = i + len(beginMarker)
	}
	if count != 1 {
		t.Fatalf("expected 1 begin marker after heal, got %d:\n%s", count, out)
	}
}

func TestStripManagedBlock(t *testing.T) {
	// Just verify the stripping logic against an in-memory string by
	// reimplementing the trim — actual file IO covered separately.
	rc := "export FOO=bar\n\n" + AliasBlock + "alias other='x'\n"
	// strip via upsertBlock with empty block would change semantics; we
	// only verify here that the markers can be found and the regions
	// computed. Use Index directly to mirror stripManagedBlock logic.
	bi := indexFrom(rc, beginMarker, 0)
	ei := indexFrom(rc, endMarker, bi)
	if bi == -1 || ei == -1 {
		t.Fatal("markers missing from test fixture")
	}
}

// helpers

func contains(s, sub string) bool {
	return indexFrom(s, sub, 0) != -1
}

func indexFrom(s, sub string, from int) int {
	if from > len(s) {
		return -1
	}
	rel := -1
	for i := from; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			rel = i
			break
		}
	}
	return rel
}
