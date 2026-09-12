package httpx

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCursorRoundTrip(t *testing.T) {
	id := uuid.New()
	ts := time.Date(2025, 7, 14, 19, 34, 12, 480_000_000, time.UTC)
	idx := int32(62)

	for name, c := range map[string]Cursor{
		"time and id": {Time: &ts, ID: &id},
		"name and id": {Name: "Doe, John", ID: &id},
		"index":       {Int: &idx},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := DecodeCursor(c.Encode())
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if (got.Time == nil) != (c.Time == nil) ||
				(got.Time != nil && !got.Time.Equal(*c.Time)) {
				t.Errorf("time: got %v want %v", got.Time, c.Time)
			}
			if got.Name != c.Name {
				t.Errorf("name: got %q want %q", got.Name, c.Name)
			}
			if (got.ID == nil) != (c.ID == nil) || (got.ID != nil && *got.ID != *c.ID) {
				t.Errorf("id: got %v want %v", got.ID, c.ID)
			}
			if (got.Int == nil) != (c.Int == nil) || (got.Int != nil && *got.Int != *c.Int) {
				t.Errorf("int: got %v want %v", got.Int, c.Int)
			}
		})
	}
}

func TestCursorIsOpaque(t *testing.T) {
	// The encoding is an implementation detail; a client that reads it will
	// break when it changes. Base64 is the smallest hint that it should not
	// be parsed.
	id := uuid.New()
	token := Cursor{Name: "Doe, John", ID: &id}.Encode()
	if token == "" {
		t.Fatal("empty token")
	}
	for _, c := range token {
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			t.Fatalf("token contains a character needing URL escaping: %q", c)
		}
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, token := range []string{"not-base64!!", "YWJj"} {
		if _, err := DecodeCursor(token); err == nil {
			t.Errorf("expected rejection of %q", token)
		}
	}
}

func TestDecodeEmptyCursorIsTheFirstPage(t *testing.T) {
	c, err := DecodeCursor("")
	if err != nil {
		t.Fatalf("empty cursor should be valid: %v", err)
	}
	if c.Time != nil || c.ID != nil || c.Int != nil || c.Name != "" {
		t.Fatal("empty cursor should carry no position")
	}
}

func TestPaginateReportsMoreWithoutACountQuery(t *testing.T) {
	// Fetching limit+1 rows is how HasMore is known. A COUNT over a table of
	// millions would cost as much as the page itself.
	rows := []int{1, 2, 3, 4}

	page, more := Paginate(rows, 3)
	if len(page) != 3 || !more {
		t.Fatalf("got %d rows, more=%v; want 3 and true", len(page), more)
	}

	page, more = Paginate(rows[:2], 3)
	if len(page) != 2 || more {
		t.Fatalf("got %d rows, more=%v; want 2 and false", len(page), more)
	}

	page, more = Paginate(rows[:3], 3)
	if len(page) != 3 || more {
		t.Fatalf("exactly one full page should not report more")
	}
}

func TestLimitIsBounded(t *testing.T) {
	// An unbounded limit is both a denial-of-service vector and an accidental
	// table scan.
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", 50, false},
		{"10", 10, false},
		{"500", 500, false},
		{"501", 0, true},
		{"0", 0, true},
		{"-1", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/?limit="+tc.raw, nil)
		got, err := Limit(r, 50, 500)
		if tc.wantErr {
			if err == nil {
				t.Errorf("limit=%q should be rejected", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("limit=%q: %v", tc.raw, err)
		}
		if got != tc.want {
			t.Errorf("limit=%q: got %d want %d", tc.raw, got, tc.want)
		}
	}
}

func TestQueryTimeAcceptsDateOrTimestamp(t *testing.T) {
	for _, raw := range []string{
		"2025-07-14",
		"2025-07-14T19:34:12Z",
		"2025-07-14T19:34:12.480123456Z",
	} {
		r := httptest.NewRequest("GET", "/?from="+raw, nil)
		got, err := QueryTime(r, "from")
		if err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if got == nil || got.Year() != 2025 || got.Month() != time.July || got.Day() != 14 {
			t.Errorf("%q parsed to %v", raw, got)
		}
	}

	r := httptest.NewRequest("GET", "/?from=yesterday", nil)
	if _, err := QueryTime(r, "from"); err == nil {
		t.Error("expected rejection of an unparseable timestamp")
	}
}

func TestQueryEnumRejectsUnknownValues(t *testing.T) {
	r := httptest.NewRequest("GET", "/?status=DELETED", nil)
	if _, err := QueryEnum(r, "status", "ACTIVE", "COMPLETED"); err == nil {
		t.Fatal("expected rejection")
	}

	r = httptest.NewRequest("GET", "/?status=active", nil)
	got, err := QueryEnum(r, "status", "ACTIVE", "COMPLETED")
	if err != nil {
		t.Fatal(err)
	}
	// Matching is case-insensitive, but the canonical form is returned so the
	// value can go straight into a query without further normalization.
	if got == nil || *got != "ACTIVE" {
		t.Fatalf("got %v, want canonical ACTIVE", got)
	}
}

func TestQueryIncludesDefaultsWhenAbsent(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	inc := QueryIncludes(r, "measurement", "prediction")
	if !inc["measurement"] || !inc["prediction"] {
		t.Fatal("absent include should apply the defaults")
	}

	r = httptest.NewRequest("GET", "/?include=measurement", nil)
	inc = QueryIncludes(r, "measurement", "prediction")
	if !inc["measurement"] || inc["prediction"] {
		t.Fatal("an explicit include should replace the defaults, not extend them")
	}
}
