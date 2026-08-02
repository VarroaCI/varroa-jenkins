package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestLineKey(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{"2.479.3", "2.479"},
		{"2.462.3", "2.462"},
		{"2.516", ""},
		{"2.479.3.1", ""},
		{"", ""},
		{"2", ""},
	}
	for _, tt := range tests {
		got := lineKey(tt.version)
		if got != tt.want {
			t.Errorf("lineKey(%q) = %q; want %q", tt.version, got, tt.want)
		}
	}
}

func TestResolveProfile_ExactWinsOverLine(t *testing.T) {
	profiles := []*v1alpha1.JenkinsVersionProfile{
		{ObjectMeta: metav1.ObjectMeta{Name: "v2-479"}, Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "v2-479-3"}, Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479.3"}},
	}
	p, kind := ResolveProfile("2.479.3", profiles)
	if kind != MatchExact {
		t.Errorf("expected MatchExact, got %v", kind)
	}
	if p == nil || p.Name != "v2-479-3" {
		t.Errorf("expected profile v2-479-3, got %v", p)
	}
}

func TestResolveProfile_LineMatch(t *testing.T) {
	profiles := []*v1alpha1.JenkinsVersionProfile{
		{ObjectMeta: metav1.ObjectMeta{Name: "v2-479"}, Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479"}},
	}
	p, kind := ResolveProfile("2.479.3", profiles)
	if kind != MatchLine {
		t.Errorf("expected MatchLine, got %v", kind)
	}
	if p == nil || p.Name != "v2-479" {
		t.Errorf("expected profile v2-479, got %v", p)
	}
}

func TestResolveProfile_WeeklyExactOnly(t *testing.T) {
	profiles := []*v1alpha1.JenkinsVersionProfile{
		{ObjectMeta: metav1.ObjectMeta{Name: "v2-516"}, Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.516"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "v2-5"}, Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.5"}},
	}
	// 2.516 is 2 segments (weekly), so lineKey returns "". Only exact match works.
	p, kind := ResolveProfile("2.516", profiles)
	if kind != MatchExact {
		t.Errorf("expected MatchExact for weekly, got %v", kind)
	}
	if p == nil || p.Name != "v2-516" {
		t.Errorf("expected profile v2-516, got %v", p)
	}
}

func TestResolveProfile_EmptyVersion(t *testing.T) {
	p, kind := ResolveProfile("", nil)
	if kind != MatchBaseline {
		t.Errorf("expected MatchBaseline for empty version, got %v", kind)
	}
	if p != nil {
		t.Errorf("expected nil profile for empty version, got %v", p)
	}
}

func TestResolveProfile_LtsVersion(t *testing.T) {
	p, kind := ResolveProfile("lts", nil)
	if kind != MatchBaseline {
		t.Errorf("expected MatchBaseline for 'lts', got %v", kind)
	}
	if p != nil {
		t.Errorf("expected nil profile for 'lts', got %v", p)
	}
}

func TestResolveProfile_UnknownVersion(t *testing.T) {
	profiles := []*v1alpha1.JenkinsVersionProfile{
		{ObjectMeta: metav1.ObjectMeta{Name: "v2-479"}, Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479"}},
	}
	p, kind := ResolveProfile("2.999.0", profiles)
	if kind != MatchBaseline {
		t.Errorf("expected MatchBaseline for unknown version, got %v", kind)
	}
	if p != nil {
		t.Errorf("expected nil profile for unknown version, got %v", p)
	}
}

func TestResolveProfile_DeterministicTiebreak(t *testing.T) {
	profiles := []*v1alpha1.JenkinsVersionProfile{
		{ObjectMeta: metav1.ObjectMeta{Name: "z-profile"}, Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479.3"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a-profile"}, Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479.3"}},
	}
	p, kind := ResolveProfile("2.479.3", profiles)
	if kind != MatchExact {
		t.Errorf("expected MatchExact, got %v", kind)
	}
	if p == nil || p.Name != "a-profile" {
		t.Errorf("expected deterministic tiebreak to 'a-profile', got %v", p)
	}
}
