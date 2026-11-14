package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// routineVectors is testdata/routine-vectors.json, a byte-for-byte copy of the shared vectors
// (surebetter-common/docs/superpowers/plans/2026-11-14-routine-vectors.json).
type routineVectors struct {
	Base    json.RawMessage `json:"base"`
	Resolve []struct {
		Name      string          `json:"name"`
		HumanMode bool            `json:"humanMode"`
		Routine   json.RawMessage `json:"routine"`
		Window    string          `json:"window"`
		At        string          `json:"at"`
		Expect    resolved        `json:"expect"`
	} `json:"resolve"`
	Valid   []routineCase `json:"valid"`
	Invalid []routineCase `json:"invalid"`
}

type routineCase struct {
	Name    string          `json:"name"`
	Routine json.RawMessage `json:"routine"`
}

// resolved is the resolution of a routine for one window at one instant (contract §2).
type resolved struct {
	Activity string `json:"activity"`
	Sport    string `json:"sport,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func loadRoutineVectors(t *testing.T) routineVectors {
	t.Helper()
	b, err := os.ReadFile("testdata/routine-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v routineVectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// routineJSON resolves a vector's routine: "base" means the base object.
func (v routineVectors) routineJSON(raw json.RawMessage) json.RawMessage {
	if string(raw) == `"base"` {
		return v.Base
	}
	return raw
}

// resolveRoutine is contract §2's resolution. The gateway never resolves a routine (the agent does);
// this test-only copy runs the vectors' resolve cases against the routine the gateway parses.
func resolveRoutine(humanMode bool, r *routine, windowID string, t time.Time) resolved {
	if !humanMode || r == nil {
		return resolved{Activity: "open"}
	}
	wall := t.In(prague)
	day := int(wall.Weekday()+6)%7 + 1 // 1 Mon … 7 Sun
	prev := (day+5)%7 + 1
	now := wall.Hour()*60 + wall.Minute()
	in := func(days []int, d int) bool {
		for _, x := range days {
			if x == d {
				return true
			}
		}
		return false
	}
	for _, s := range r.Sleep {
		from, _ := clockMinutes(s.From)
		to, _ := clockMinutes(s.To)
		if to > from {
			if in(s.Days, day) && now >= from && now < to {
				return resolved{Activity: "inactive", Reason: "sleep"}
			}
			continue
		}
		if (in(s.Days, day) && now >= from) || (in(s.Days, prev) && now < to) {
			return resolved{Activity: "inactive", Reason: "sleep"}
		}
	}
	for _, b := range r.Windows[windowID].Blocks {
		from, _ := clockMinutes(b.From)
		to, _ := clockMinutes(b.To)
		if in(b.Days, day) && now >= from && now < to {
			out := resolved{Activity: b.Activity}
			if b.Sport != nil {
				out.Sport = *b.Sport
			}
			if b.Activity == "inactive" {
				out.Reason = "inactive"
			}
			return out
		}
	}
	return resolved{Activity: "open"}
}

func TestRoutine_Vectors(t *testing.T) {
	v := loadRoutineVectors(t)
	if len(v.Valid) == 0 || len(v.Invalid) == 0 || len(v.Resolve) == 0 {
		t.Fatal("empty vectors")
	}
	for _, c := range v.Valid {
		if _, ok := routineOf(v.routineJSON(c.Routine)); !ok {
			t.Errorf("valid %q refused", c.Name)
		}
	}
	for _, c := range v.Invalid {
		if _, ok := routineOf(v.routineJSON(c.Routine)); ok {
			t.Errorf("invalid %q accepted", c.Name)
		}
	}
	for _, c := range v.Resolve {
		var r *routine
		if string(c.Routine) != "null" {
			var ok bool
			if r, ok = routineOf(v.routineJSON(c.Routine)); !ok {
				t.Fatalf("%s: routine refused", c.Name)
			}
		}
		at, err := time.Parse(time.RFC3339, c.At)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if got := resolveRoutine(c.HumanMode, r, c.Window, at); got != c.Expect {
			t.Errorf("resolve %q: %+v, want %+v", c.Name, got, c.Expect)
		}
	}
}

// minimalRoutine is a valid routine with sleep, windows and the trigger replaced by the given JSON
// (each "" = the default: [], {}, on 2–5).
func minimalRoutine(sleep, windows, trigger string) string {
	if sleep == "" {
		sleep = `[]`
	}
	if windows == "" {
		windows = `{}`
	}
	if trigger == "" {
		trigger = `{"on":true,"minMin":2,"maxMin":5}`
	}
	return `{"version":1,"tz":"Europe/Prague","sleep":` + sleep + `,"windows":` + windows + `,"triggers":{"afterBetCheckMyBets":` + trigger + `}}`
}

// blocks is a windows object with one window w1 holding the given blocks.
func blocks(bs ...string) string { return `{"w1":{"blocks":[` + strings.Join(bs, ",") + `]}}` }

func repeatJSON(n int, f func(i int) string) string {
	out := make([]string, n)
	for i := range out {
		out[i] = f(i)
	}
	return strings.Join(out, ",")
}

func TestRoutine_Limits(t *testing.T) {
	block := func(fields string) string {
		return `{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"mybets"` + fields + `}`
	}
	sleep := func(from, to string) string { return `[{"days":[1],"from":"` + from + `","to":"` + to + `"}]` }
	manyBlocks := func(n int) string {
		return `{"w1":{"blocks":[` + repeatJSON(n, func(i int) string {
			return fmt.Sprintf(`{"id":"b%d","days":[%d],"from":"%02d:%02d","to":"%02d:%02d","activity":"open"}`,
				i, i%7+1, i/7/2, i/7%2*30, i/7/2, i/7%2*30+15)
		}) + `]}}`
	}
	manyWindows := func(n int) string {
		return `{` + repeatJSON(n, func(i int) string { return fmt.Sprintf(`"w-%d":{"blocks":[]}`, i) }) + `}`
	}
	manySleep := func(n int) string {
		return `[` + repeatJSON(n, func(int) string { return `{"days":[1],"from":"23:00","to":"07:00"}` }) + `]`
	}
	valid := map[string]string{
		"sleep to 24:00":        minimalRoutine(sleep("22:00", "24:00"), "", ""),
		"sleep from 00:00":      minimalRoutine(sleep("00:00", "06:00"), "", ""),
		"sleep all day":         minimalRoutine(sleep("00:00", "24:00"), "", ""),
		"block 00:00–24:00":     minimalRoutine("", `{"w1":{"blocks":[{"id":"a","days":[7],"from":"00:00","to":"24:00","activity":"open"}]}}`, ""),
		"20 sleep periods":      minimalRoutine(manySleep(20), "", ""),
		"32 windows":            minimalRoutine("", manyWindows(32), ""),
		"200 blocks":            minimalRoutine("", manyBlocks(200), ""),
		"sport at 40":           minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":"`+strings.Repeat("s", 40)+`"}`), ""),
		"sport with _ and -":    minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"prematch","sport":"ice_hockey-2"}`), ""),
		"block id 16":           minimalRoutine("", blocks(`{"id":"`+strings.Repeat("a", 16)+`","days":[1],"from":"08:00","to":"09:00","activity":"open"}`), ""),
		"window id 40":          minimalRoutine("", `{"`+strings.Repeat("w", 40)+`":{"blocks":[]}}`, ""),
		"trigger 60–60":         minimalRoutine("", "", `{"on":false,"minMin":60,"maxMin":60}`),
		"days 1–7 in any order": minimalRoutine("", blocks(`{"id":"a","days":[7,1,3,2,6,5,4],"from":"08:00","to":"09:00","activity":"open"}`), ""),
	}
	for name, body := range valid {
		if _, ok := routineOf([]byte(body)); !ok {
			t.Errorf("%s: refused", name)
		}
	}
	invalid := map[string]string{
		"not JSON":              `{"version":1`,
		"an array":              `[]`,
		"null":                  `null`,
		"21 sleep periods":      minimalRoutine(manySleep(21), "", ""),
		"33 windows":            minimalRoutine("", manyWindows(33), ""),
		"201 blocks":            minimalRoutine("", manyBlocks(201), ""),
		"sport over 40":         minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":"`+strings.Repeat("s", 41)+`"}`), ""),
		"sport uppercase":       minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":"Football"}`), ""),
		"sport empty":           minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":""}`), ""),
		"sport null on live":    minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":null}`), ""),
		"sport on open":         minimalRoutine("", blocks(block(`,"sport":"any"`)), ""),
		"block id 17":           minimalRoutine("", blocks(`{"id":"`+strings.Repeat("a", 17)+`","days":[1],"from":"08:00","to":"09:00","activity":"open"}`), ""),
		"block id with dash":    minimalRoutine("", blocks(`{"id":"a-1","days":[1],"from":"08:00","to":"09:00","activity":"open"}`), ""),
		"block from 24:00":      minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"24:00","to":"24:00","activity":"open"}`), ""),
		"minutes 60":            minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:60","to":"09:00","activity":"open"}`), ""),
		"hour 25":               minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"25:00","activity":"open"}`), ""),
		"24:01":                 minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"24:01","activity":"open"}`), ""),
		"day 8":                 minimalRoutine("", blocks(`{"id":"a","days":[8],"from":"08:00","to":"09:00","activity":"open"}`), ""),
		"day 1.5":               minimalRoutine("", blocks(`{"id":"a","days":[1.5],"from":"08:00","to":"09:00","activity":"open"}`), ""),
		"block without days":    minimalRoutine("", blocks(`{"id":"a","from":"08:00","to":"09:00","activity":"open"}`), ""),
		"block without id":      minimalRoutine("", blocks(`{"days":[1],"from":"08:00","to":"09:00","activity":"open"}`), ""),
		"blocks null":           minimalRoutine("", `{"w1":{"blocks":null}}`, ""),
		"window without blocks": minimalRoutine("", `{"w1":{}}`, ""),
		"window id 41":          minimalRoutine("", `{"`+strings.Repeat("w", 41)+`":{"blocks":[]}}`, ""),
		"sleep from 24:00":      minimalRoutine(sleep("24:00", "06:00"), "", ""),
		"sleep 00:00 to 00:00":  minimalRoutine(sleep("00:00", "00:00"), "", ""),
		"sleep without days":    minimalRoutine(`[{"from":"23:00","to":"07:00"}]`, "", ""),
		"sleep day repeated":    minimalRoutine(`[{"days":[2,2],"from":"23:00","to":"07:00"}]`, "", ""),
		"sleep null":            strings.Replace(minimalRoutine("", "", ""), `"sleep":[]`, `"sleep":null`, 1),
		"no sleep":              strings.Replace(minimalRoutine("", "", ""), `"sleep":[],`, ``, 1),
		"no windows":            strings.Replace(minimalRoutine("", "", ""), `"windows":{},`, ``, 1),
		"windows an array":      minimalRoutine("", `[]`, ""),
		"no version":            strings.Replace(minimalRoutine("", "", ""), `"version":1,`, ``, 1),
		"no tz":                 strings.Replace(minimalRoutine("", "", ""), `"tz":"Europe/Prague",`, ``, 1),
		"trigger without on":    minimalRoutine("", "", `{"minMin":2,"maxMin":5}`),
		"trigger without min":   minimalRoutine("", "", `{"on":true,"maxMin":5}`),
		"trigger without max":   minimalRoutine("", "", `{"on":true,"minMin":2}`),
		"minMin 0":              minimalRoutine("", "", `{"on":true,"minMin":0,"maxMin":5}`),
		"minMin 2.5":            minimalRoutine("", "", `{"on":true,"minMin":2.5,"maxMin":5}`),
		"trigger null":          strings.Replace(minimalRoutine("", "", ""), `"afterBetCheckMyBets":{"on":true,"minMin":2,"maxMin":5}`, `"afterBetCheckMyBets":null`, 1),
		"triggers empty":        strings.Replace(minimalRoutine("", "", ""), `{"afterBetCheckMyBets":{"on":true,"minMin":2,"maxMin":5}}`, `{}`, 1),
		"overlap across days":   minimalRoutine("", blocks(block(""), `{"id":"b","days":[3,1],"from":"07:00","to":"08:01","activity":"open"}`), ""),
		"overlap contained":     minimalRoutine("", blocks(block(""), `{"id":"b","days":[1],"from":"08:15","to":"08:30","activity":"open"}`), ""),
	}
	for name, body := range invalid {
		if _, ok := routineOf([]byte(body)); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The stored routine is the parsed one re-encoded: only the known keys, sport only where it is set.
func TestRoutine_Encoding(t *testing.T) {
	in := `{"tz":"Europe/Prague","version":1,"extra":true,"sleep":[{"days":[1],"from":"23:00","to":"07:00","x":1}],` +
		`"windows":{"w2":{"blocks":[]},"w1":{"blocks":[{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":"any"},` +
		`{"id":"b","days":[2],"from":"08:00","to":"09:00","activity":"open"}]}},"triggers":{"afterBetCheckMyBets":{"on":true,"minMin":2,"maxMin":5}}}`
	r, ok := routineOf([]byte(in))
	if !ok {
		t.Fatal("refused")
	}
	want := `{"version":1,"tz":"Europe/Prague","sleep":[{"days":[1],"from":"23:00","to":"07:00"}],` +
		`"windows":{"w1":{"blocks":[{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":"any"},` +
		`{"id":"b","days":[2],"from":"08:00","to":"09:00","activity":"open"}]},"w2":{"blocks":[]}},"triggers":{"afterBetCheckMyBets":{"on":true,"minMin":2,"maxMin":5}}}`
	if got := mustJSON(r); got != want {
		t.Fatalf("encoded:\n got %s\nwant %s", got, want)
	}
}
