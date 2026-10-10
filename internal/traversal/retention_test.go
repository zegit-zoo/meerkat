package traversal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// retention_test.go pins meerkat-mob#45 items 2 and 3: warm start reads
// only temperature records, and retention runs on schedule, keeps going
// past a failed day and counts what it did.

// memSink is an in-memory Sink that records which objects were read and
// can be told to fail deleting one day.
type memSink struct {
	mu      sync.Mutex
	objects map[string]map[string][]byte // day -> name -> body
	read    []string
	failDay string
}

func newMemSink() *memSink { return &memSink{objects: map[string]map[string][]byte{}} }

func (m *memSink) Put(_ context.Context, key string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rel := strings.TrimPrefix(key, Prefix)
	day, name, _ := strings.Cut(rel, "/")
	if m.objects[day] == nil {
		m.objects[day] = map[string][]byte{}
	}
	m.objects[day][name] = body
	return nil
}

func (m *memSink) Days(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for d := range m.objects {
		out = append(out, d)
	}
	return out, nil
}

func (m *memSink) DeleteDay(_ context.Context, day string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if day == m.failDay {
		return errors.New("access denied")
	}
	delete(m.objects, day)
	return nil
}

func (m *memSink) ReadDay(_ context.Context, day string, keep func(string) bool) ([][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out [][]byte
	for name, b := range m.objects[day] {
		if keep != nil && !keep(name) {
			continue
		}
		m.read = append(m.read, name)
		out = append(out, b)
	}
	return out, nil
}

func (m *memSink) days() string {
	d, _ := m.Days(context.Background())
	sort.Strings(d)
	return strings.Join(d, ",")
}

var testKey = []byte("not a secret, a test key long enough")

func TestReadTemperatures_ReadsNoSessionEntries(t *testing.T) {
	ctx := context.Background()
	sink := newMemSink()
	l := NewLog(sink, testKey, 0)
	for i := range 5 {
		if _, err := l.Record(ctx, Entry{Session: fmt.Sprint("s", i), Outcome: "found"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.RecordTemperatures(ctx, map[string]Temperature{"notes": {Temperature: 3}}); err != nil {
		t.Fatal(err)
	}
	temps, err := l.ReadTemperatures(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if temps[l.Hash("notes")].Temperature != 3 {
		t.Errorf("temperatures = %+v", temps)
	}
	for _, name := range sink.read {
		if !IsTemperatureObject(name) {
			t.Errorf("warm start fetched session entry %s", name)
		}
	}
	if len(sink.read) != 1 {
		t.Errorf("read %d objects, want the 1 temperature record", len(sink.read))
	}

	// And the librarian's session read skips temperatures.
	sink.read = nil
	entries, err := l.ReadSessions(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Errorf("ReadSessions = %d entries, want 5", len(entries))
	}
	for _, name := range sink.read {
		if IsTemperatureObject(name) {
			t.Errorf("ReadSessions fetched temperature record %s", name)
		}
	}
}

// seedDays puts one object in each of the given days.
func seedDays(t *testing.T, sink *memSink, days ...string) {
	t.Helper()
	for _, d := range days {
		if err := sink.Put(context.Background(), Prefix+d+"/x.json", []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrune_ContinuesPastAFailedDayAndCounts(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	sink := newMemSink()
	seedDays(t, sink, "2026-01-01", "2026-01-02", "2026-01-03", "2026-10-06")
	sink.failDay = "2026-01-01" // the OLDEST day fails: later ones must still go
	l := NewLog(sink, testKey, 30)
	l.now = func() time.Time { return now }
	var got []PruneResult
	l.OnPrune(func(r PruneResult) { got = append(got, r) })

	err := l.Prune(context.Background())
	if err == nil || !strings.Contains(err.Error(), "2026-01-01") {
		t.Fatalf("Prune = %v, want the failed day reported", err)
	}
	if d := sink.days(); d != "2026-01-01,2026-10-06" {
		t.Errorf("days after prune = %q: a failed day blocked later deletions", d)
	}
	st := l.PruneStats()
	if st.Runs != 1 || st.DaysDeleted != 2 || st.DaysFailed != 1 || st.LastError == "" {
		t.Errorf("stats = %+v", st)
	}
	if len(got) != 1 || got[0].Deleted != 2 || got[0].Failed != 1 {
		t.Errorf("observer saw %+v", got)
	}

	// The next pass retries it.
	sink.failDay = ""
	if err := l.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := sink.days(); d != "2026-10-06" {
		t.Errorf("days after retry = %q", d)
	}
	if st := l.PruneStats(); st.LastError != "" || st.DaysDeleted != 3 {
		t.Errorf("stats after a clean pass = %+v", st)
	}
}

type failingDays struct{ *memSink }

func (failingDays) Days(context.Context) ([]string, error) { return nil, errors.New("listing denied") }

func TestPrune_CountsAFailedListing(t *testing.T) {
	l := NewLog(failingDays{newMemSink()}, testKey, 30)
	if err := l.Prune(context.Background()); err == nil {
		t.Fatal("a failed listing was not reported")
	}
	if st := l.PruneStats(); st.ListFailures != 1 {
		t.Errorf("stats = %+v", st)
	}
}

// A server that flushes temperatures but receives no reports still
// expires old days.
func TestRecordTemperatures_Prunes(t *testing.T) {
	sink := newMemSink()
	seedDays(t, sink, "2020-01-01")
	l := NewLog(sink, testKey, 30)
	if err := l.RecordTemperatures(context.Background(), map[string]Temperature{"notes": {Temperature: 1}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sink.days(), "2020-01-01") {
		t.Errorf("an expired day survived a temperature flush: %s", sink.days())
	}
}

func TestRunRetention_PrunesOnATicker(t *testing.T) {
	sink := newMemSink()
	l := NewLog(sink, testKey, 30)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.RunRetention(ctx, 5*time.Millisecond); close(done) }()
	seedDays(t, sink, "2020-01-01")
	deadline := time.Now().Add(5 * time.Second)
	for strings.Contains(sink.days(), "2020-01-01") {
		if time.Now().After(deadline) {
			t.Fatal("the ticker never pruned")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

// s3Listing serves a ListObjectsV2 with delimiter "/" over two pages of
// common prefixes, the way S3 does past 1,000 entries.
func s3Listing(t *testing.T) *httptest.Server {
	t.Helper()
	const page = `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>b</Name><Prefix>telemetry/paths/</Prefix><Delimiter>/</Delimiter><KeyCount>1</KeyCount><MaxKeys>1</MaxKeys><IsTruncated>%t</IsTruncated>%s<CommonPrefixes><Prefix>telemetry/paths/%s/</Prefix></CommonPrefixes></ListBucketResult>`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("continuation-token") == "" {
			_, _ = fmt.Fprintf(w, page, true, "<NextContinuationToken>next</NextContinuationToken>", "2026-01-01")
			return
		}
		_, _ = fmt.Fprintf(w, page, false, "", "2026-01-02")
	}))
}

func TestS3Sink_DaysPagesTheListing(t *testing.T) {
	srv := s3Listing(t)
	defer srv.Close()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", os.DevNull)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	sink, err := NewS3Sink(context.Background(), &Config{Bucket: "b", Endpoint: srv.URL, Region: "test", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	days, err := sink.Days(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(days, ",") != "2026-01-01,2026-01-02" {
		t.Errorf("Days = %v, want both pages", days)
	}
}

func TestLocalSink_ReadDayFiltersByName(t *testing.T) {
	dir := t.TempDir()
	day := filepath.Join(dir, "2026-10-07")
	if err := os.MkdirAll(day, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"1-temperature.json", "2-abcd.json"} {
		if err := os.WriteFile(filepath.Join(day, n), []byte(`{"n":"`+n+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := (&localSink{dir: dir}).ReadDay(context.Background(), "2026-10-07", IsTemperatureObject)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(string(got[0]), "temperature") {
		t.Errorf("ReadDay = %q", got)
	}
}
