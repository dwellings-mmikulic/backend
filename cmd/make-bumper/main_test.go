package main

import (
	"testing"

	"github.com/dwellingtw/backend/internal/bumper"
)

func TestApplyKind_SlateDefaultsAndLimits(t *testing.T) {
	// A slate takes the slate caption and length unless the flags were set.
	s := bumper.Spec{Caption: bumper.DefaultCaption, Seconds: bumper.DefaultSeconds}
	if err := applyKind(&s, "slate", false, false); err != nil {
		t.Fatal(err)
	}
	if s.Kind != "slate" || s.Caption != bumper.SlateCaption || s.Seconds != bumper.SlateSeconds {
		t.Errorf("slate spec = %+v", s)
	}
	s = bumper.Spec{Caption: "mine", Seconds: 120}
	if err := applyKind(&s, "slate", true, true); err != nil {
		t.Fatal(err)
	}
	if s.Caption != "mine" || s.Seconds != 120 {
		t.Errorf("explicit flags overridden: %+v", s)
	}
	// A slate may run up to three minutes, a bumper only one.
	s = bumper.Spec{Seconds: 180}
	if err := applyKind(&s, "slate", true, true); err != nil {
		t.Errorf("180 s slate rejected: %v", err)
	}
	s = bumper.Spec{Seconds: 181}
	if err := applyKind(&s, "slate", true, true); err == nil {
		t.Error("181 s slate accepted")
	}
	s = bumper.Spec{Seconds: 61}
	if err := applyKind(&s, "bumper", true, true); err == nil {
		t.Error("61 s bumper accepted")
	}
	s = bumper.Spec{Seconds: 2}
	if err := applyKind(&s, "slate", true, true); err == nil {
		t.Error("2 s slate accepted")
	}
}

func TestApplyKind_BumperIsUnchanged(t *testing.T) {
	s := bumper.Spec{Caption: bumper.DefaultCaption, Seconds: bumper.DefaultSeconds}
	if err := applyKind(&s, "bumper", false, false); err != nil {
		t.Fatal(err)
	}
	if s.Kind != "bumper" || s.Caption != bumper.DefaultCaption || s.Seconds != bumper.DefaultSeconds {
		t.Errorf("bumper spec = %+v", s)
	}
	if err := applyKind(&s, "promo", false, false); err == nil {
		t.Error("unknown kind accepted")
	}
}
