package main

import (
	"strings"
	"time"
	_ "time/tzdata" // Europe/Prague without the OS zone files (tests, any image)
)

// prague is the zone of the bots' calendar day (the per-day limit, "today", ?day=).
var prague = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		panic("Europe/Prague: " + err.Error()) // embedded by time/tzdata: cannot happen
	}
	return loc
}()

// pragueDayOf returns the Prague calendar day holding t: [its midnight, the next midnight). A
// daylight-saving day is 23 or 25 hours long.
func pragueDayOf(t time.Time) (from, to time.Time) {
	y, m, d := t.In(prague).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, prague), time.Date(y, m, d+1, 0, 0, 0, 0, prague)
}

// parsePragueDay reads a YYYY-MM-DD day (a real date) as a Prague calendar day.
func parsePragueDay(s string) (from, to time.Time, ok bool) {
	t, err := time.ParseInLocation("2006-01-02", s, prague)
	if err != nil || t.Format("2006-01-02") != s {
		return time.Time{}, time.Time{}, false
	}
	from, to = pragueDayOf(t)
	return from, to, true
}

// bookLabels are the bookmakers' display names, as the app shows them (frontend config/bookmakers.ts).
var bookLabels = map[string]string{
	"tipsport": "Tipsport", "betano": "Betano", "fortuna": "Fortuna", "sazkabet": "SazkaBet", "merkurxtip": "MerkurXtip",
	"synottip": "SynotTip", "kingsbet": "Kingsbet", "chance": "Chance", "pinnacle": "Pinnacle",
}

// bookLabel is a bookmaker's display name; an unknown one is capitalised.
func bookLabel(book string) string {
	if l, ok := bookLabels[book]; ok {
		return l
	}
	if book == "" {
		return ""
	}
	return strings.ToUpper(book[:1]) + book[1:]
}
