package client

import (
	"testing"
	"time"
)

func TestGranolaSessionIsStablePerBrief(t *testing.T) {
	now := time.Now()
	a := GranolaSession("brief one", now)
	b := GranolaSession("brief one", now.Add(time.Hour))
	c := GranolaSession("brief two", now)
	if a.ID != b.ID {
		t.Fatalf("same brief got two ids: %s %s", a.ID, b.ID)
	}
	if a.ID == c.ID {
		t.Fatalf("different briefs share id %s", a.ID)
	}
	if a.Source != GranolaSource || len(a.Turns) != 1 || a.Turns[0].Text != "brief one" {
		t.Fatalf("bad session: %+v", a)
	}
}

func TestGranolaConnected(t *testing.T) {
	var g *Granola
	if g.Connected() {
		t.Fatal("nil granola is connected")
	}
	if (&Granola{ClientID: "x"}).Connected() {
		t.Fatal("a registered client with no token is connected")
	}
	if !(&Granola{RefreshToken: "r"}).Connected() {
		t.Fatal("a refresh token should count as connected")
	}
}
