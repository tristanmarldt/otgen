package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func testTUI(t *testing.T) *tui {
	t.Helper()
	app := NewApp("/dev/null", 0)
	app.cfg = normalizeConfig(Config{
		Endpoint:   "https://example.live.dynatrace.com/api/v2/otlp",
		Token:      "dt0c01.TOKEN",
		Attributes: map[string]AttrValue{"dt.security_context": strAttrVal("ctx")},
		Services: []Service{{
			Name: "svc", Template: "http-server", InfraTemplate: "k8s",
			SpanKind: "server", FailureRate: 5, Interval: 5, ChildSpans: 3, Enabled: true,
		}},
	})
	m := NewTUIModel(app)
	m.width, m.height = 100, 32
	return m
}

// TestUntouchedTemplateAttrsAreNotPersisted guards against template defaults
// being frozen into the config as explicit overrides on every save, which would
// stop the service from tracking the template.
func TestUntouchedTemplateAttrsAreNotPersisted(t *testing.T) {
	m := testTUI(t)
	m.loadServiceFields(0)

	svc := m.buildServiceFromFields()
	if len(svc.Attributes) != 0 {
		t.Errorf("untouched template attrs persisted as overrides: %v", svc.Attributes)
	}
	if len(svc.SpanAttrs) != 0 {
		t.Errorf("untouched span attrs persisted as overrides: %v", svc.SpanAttrs)
	}
	if m.hasUnsavedChanges() {
		t.Error("freshly loaded service reports unsaved changes")
	}
}

func TestServiceTabCompletionWaitsForSave(t *testing.T) {
	m := testTUI(t)
	m.loadServiceFields(0)
	m.screen, m.tabActive = screenServiceEdit, false
	m.fFailure = "25"
	m.form = m.makeServiceTabForm(0)

	m.commitForm()

	if got := m.app.GetConfig().Services[0].FailureRate; got != 5 {
		t.Fatalf("tab completion saved failure rate %d before s, want 5", got)
	}
	if !m.hasUnsavedChanges() {
		t.Fatal("tab completion should leave unsaved changes in the selector")
	}
}

func TestNewServiceOpensSettings(t *testing.T) {
	m := testTUI(t)
	m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})

	if m.screen != screenServiceEdit || m.tabActive || m.form == nil {
		t.Fatalf("new service did not open the Settings form: screen=%v tabActive=%v form=%v", m.screen, m.tabActive, m.form != nil)
	}
	if m.fName != defaultServiceNamePrefix {
		t.Fatalf("new service name = %q, want %q", m.fName, defaultServiceNamePrefix)
	}
}

func TestHelpFitsStandardTerminal(t *testing.T) {
	m := testTUI(t)
	m.width, m.height, m.screen = 80, 24, screenHelp
	if got := strings.Count(m.View(), "\n") + 1; got > m.height {
		t.Fatalf("help renders %d rows in a %d-row terminal", got, m.height)
	}
}

func TestSettingsSummaryLeavesEnabledToOverview(t *testing.T) {
	m := testTUI(t)
	if got := m.serviceTabSummaries()[0]; strings.Contains(got, "enabled") || strings.Contains(got, "disabled") {
		t.Fatalf("Settings summary duplicates overview toggle: %q", got)
	}
}

func TestServiceFormKeepsMeshDescriptionReadable(t *testing.T) {
	m := testTUI(t)
	m.loadServiceFields(0)
	m.width, m.height, m.screen = 80, 24, screenServiceEdit
	m.form = m.makeServiceTabForm(tabService)
	m.form.Init()
	view := stripANSI(m.form.View())
	if !strings.Contains(view, "Istio mesh") {
		t.Errorf("service form missing %q:\n%s", "Istio mesh", view)
	}
	if strings.Contains(view, "meshspan") {
		t.Fatalf("mesh label and description run together:\n%s", view)
	}
}

func TestGlobalFormLoadsSavedTokenForEditing(t *testing.T) {
	m := testTUI(t)
	m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if m.gToken != m.cfg.Token {
		t.Fatalf("global token = %q, want saved token loaded for editing", m.gToken)
	}
}

func TestGlobalFormCanClearSavedToken(t *testing.T) {
	m := testTUI(t)
	m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	m.gToken = ""
	m.commitGlobal()

	if got := m.app.GetConfig().Token; got != "" {
		t.Fatalf("saved token = %q after clearing the field", got)
	}
}

func TestAttributeOverridesSurviveTemplateSwitch(t *testing.T) {
	m := testTUI(t)
	m.loadServiceFields(0)

	m.fAttrs = "my.custom=1\nmy.flag=true"
	m.fInfraTemplate = "ecs"

	svc := m.buildServiceFromFields()
	if len(svc.Attributes) != 2 {
		t.Fatalf("expected 2 persisted attrs, got %v", svc.Attributes)
	}
	if svc.Attributes["my.custom"] != intAttrVal(1) {
		t.Errorf("my.custom typed wrongly: %+v", svc.Attributes["my.custom"])
	}
	if svc.Attributes["my.flag"] != boolAttrVal(true) {
		t.Errorf("my.flag typed wrongly: %+v", svc.Attributes["my.flag"])
	}
}

// TestEnvOverridesAreSurfaced checks that an env-var override is visible in the
// header rather than silently beating whatever the user typed in the UI.
func TestEnvOverridesAreSurfaced(t *testing.T) {
	t.Setenv(envEndpoint, "https://env.example.com/api/v2/otlp")
	t.Setenv(envToken, "dt0c01.ENV")

	m := testTUI(t)
	header := m.renderHeader()
	for _, want := range []string{"env.example.com", "[env]"} {
		if !strings.Contains(header, want) {
			t.Errorf("header missing %q, got:\n%s", want, header)
		}
	}
}

// TestTemplateSelectsRenderEveryOption guards against reintroducing
// Select.Height(): huh v1.0.0 pins viewport.YOffset to the selected index on
// every Update when a height is set, which both scrolls the list under a
// stationary cursor and hides the options past the fold.
func TestTemplateSelectsRenderEveryOption(t *testing.T) {
	cases := []struct {
		tab      int
		lastOpt  string
		firstOpt string
	}{
		{tabSpans, "gRPC", "None (generic)"},
		// Infrastructure: two-field hierarchy. Check the category select spans
		// None (first) through Other (last) without clipping.
		{tabInfrastructure, "Other", "None"},
	}

	m := testTUI(t)
	m.loadServiceFields(0)
	for _, tc := range cases {
		m.editTab = tc.tab
		m.tabActive = false
		m.screen = screenServiceEdit
		m.form = m.makeServiceTabForm(tc.tab)
		m.form.Init()

		view := m.View()
		for _, want := range []string{tc.firstOpt, tc.lastOpt} {
			if !strings.Contains(view, want) {
				t.Errorf("tab %d does not render %q — the select is being clipped by a viewport:\n%s",
					tc.tab+1, want, view)
			}
		}
	}
}

// TestEditorTabsFitStandardTerminal keeps every editor tab inside a classic
// 24-row terminal. huh cannot scroll a form that overflows its group, so a tab
// taller than the terminal has fields the user can focus but never see.
func TestEditorTabsFitStandardTerminal(t *testing.T) {
	const rows = 24

	m := testTUI(t)
	m.width, m.height = 100, rows
	m.loadServiceFields(0)
	m.screen = screenServiceEdit

	for i, name := range serviceTabNames {
		m.tabActive, m.editTab = false, i
		m.form = m.makeServiceTabForm(i)
		m.form.Init()
		if got := strings.Count(m.View(), "\n") + 1; got > rows {
			t.Errorf("tab %d (%s) renders %d rows, exceeding a %d-row terminal", i+1, name, got, rows)
		}
	}

	m.tabActive = true
	if got := strings.Count(m.View(), "\n") + 1; got > rows {
		t.Errorf("tab selector renders %d rows, exceeding a %d-row terminal", got, rows)
	}

	m.screen = screenGlobal
	m.form = m.makeGlobalForm()
	m.form.Init()
	if got := strings.Count(m.View(), "\n") + 1; got > rows {
		t.Errorf("global form renders %d rows, exceeding a %d-row terminal", got, rows)
	}
}

// TestBracketNavigationCyclesTabs checks that ] and [ move forward and backward
// through editor tabs while the tab selector is focused (ctrl+r was removed and
// replaced with ] / [ so it stays out of huh's key space).
func TestBracketNavigationCyclesTabs(t *testing.T) {
	m := testTUI(t)
	m.loadServiceFields(0)
	m.screen, m.tabActive, m.editTab = screenServiceEdit, true, 0

	m.updateServiceSelector(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("]")})
	if m.editTab != 1 {
		t.Fatalf("] moved to tab %d, want 1", m.editTab)
	}

	m.updateServiceSelector(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("[")})
	if m.editTab != 0 {
		t.Fatalf("[ moved to tab %d, want 0", m.editTab)
	}

	// [ at the first tab must not underflow.
	m.updateServiceSelector(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("[")})
	if m.editTab != 0 {
		t.Fatalf("[ from tab 0 moved to tab %d, want 0", m.editTab)
	}

	// ] at the last tab must not overflow.
	m.editTab = len(serviceTabNames) - 1
	m.updateServiceSelector(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("]")})
	if m.editTab != len(serviceTabNames)-1 {
		t.Fatalf("] from last tab moved to tab %d, want %d", m.editTab, len(serviceTabNames)-1)
	}
}

func TestNumericNavigationReachesAllTabs(t *testing.T) {
	m := testTUI(t)
	m.loadServiceFields(0)
	m.screen, m.tabActive = screenServiceEdit, true
	for i := range serviceTabNames {
		key := string(rune('1' + i))
		m.updateServiceSelector(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if m.editTab != i || m.form == nil {
			t.Fatalf("key %q selected tab %d, want %d", key, m.editTab, i)
		}
		m.tabActive = true
	}
}

func TestCallsAndMetricsLogsRoundTrip(t *testing.T) {
	m := testTUI(t)
	m.loadServiceFields(0)
	m.fDownstream = []string{"payment-svc", "inventory-svc"}
	m.fMetricType = "histogram"
	m.fMetricName = "latency"
	m.fMetricUnit = "ms"
	m.fLogSeverity = "warn"
	svc := m.buildServiceFromFields()
	if len(svc.DownstreamCalls) != 2 || svc.DownstreamCalls[0] != "payment-svc" {
		t.Fatalf("downstream calls = %+v", svc.DownstreamCalls)
	}
	if svc.Metric == nil || svc.Metric.Type != "histogram" || svc.Metric.Name != "latency" || svc.Metric.Unit != "ms" {
		t.Fatalf("metric config = %+v", svc.Metric)
	}
	if svc.LogSeverity != "warn" {
		t.Fatalf("log severity = %q", svc.LogSeverity)
	}
}

func TestRenameUpdatesInboundCalls(t *testing.T) {
	m := testTUI(t)
	m.cfg.Services = append(m.cfg.Services, Service{Name: "caller", SpanKind: "server", Interval: 5, DownstreamCalls: []string{"svc"}})
	m.loadServiceFields(0)
	m.fName = "renamed"
	m.screen, m.tabActive = screenServiceEdit, true
	m.commitService()
	if got := m.app.GetConfig().Services[1].DownstreamCalls[0]; got != "renamed" {
		t.Fatalf("renamed call target = %q, want renamed", got)
	}
}

func TestDeleteReferencedServiceIsBlocked(t *testing.T) {
	m := testTUI(t)
	m.cfg.Services = append(m.cfg.Services, Service{Name: "caller", SpanKind: "server", Interval: 5, DownstreamCalls: []string{"svc"}})
	m.app.cfg = m.cfg
	m.cursor = 0
	m.fDeleteConfirmed = true
	m.commitDelete()
	if len(m.app.GetConfig().Services) != 2 {
		t.Fatal("referenced service was deleted")
	}
	if !m.flashErr || !strings.Contains(m.flash, "referenced by caller") {
		t.Fatalf("delete error = %q", m.flash)
	}
}

// TestTabBarFitsNarrowTerminals guards the tab bar against wrapping.
func TestTabBarFitsNarrowTerminals(t *testing.T) {
	m := testTUI(t)
	for _, w := range []int{40, 60, 80, 100, 160} {
		m.width = w
		for active := range serviceTabNames {
			bar := strings.SplitN(m.tabBar(active), "\n", 2)[0]
			if got := len([]rune(stripANSI(bar))); got > w {
				t.Errorf("tab bar is %d cols at width %d (active %d): %q", got, w, active, bar)
			}
		}
	}
}

// TestHelpLineFitsNarrowTerminals guards the help footer against wrapping.
func TestHelpLineFitsNarrowTerminals(t *testing.T) {
	m := testTUI(t)
	for _, w := range []int{40, 62, 80, 100, 160} {
		m.width = w
		line := m.renderHelp()
		if got := len([]rune(stripANSI(line))); got > w {
			t.Errorf("help line is %d cols at width %d: %q", got, w, line)
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEscape = true
		case inEscape && r == 'm':
			inEscape = false
		case !inEscape:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestMetricsTabFitsWithInheritedNote covers the case
// TestEditorTabsFitStandardTerminal misses: its fixture has no infra metrics
// and no mesh, so the "Also emitted" note is empty. With both present the note
// is at its longest, and huh cannot scroll a group that overflows its terminal.
func TestMetricsTabFitsWithInheritedNote(t *testing.T) {
	// 20 rows matters as much as 24: it is short enough to force the note's
	// truncation path, where an over-long "+N more" marker re-wraps and costs
	// back the row the trim was meant to save.
	for _, rows := range []int{20, 24, 30} {
		testMetricsTabFits(t, rows)
	}
}

func testMetricsTabFits(t *testing.T, rows int) {
	t.Helper()
	for _, tc := range []struct {
		name string
		tmpl string
		mesh bool
	}{
		{"otel-host", "otel-host", false},
		{"otel-host-process", "otel-host-process", false},
		{"otel-host-process + mesh", "otel-host-process", true},
	} {
		t.Run(fmt.Sprintf("%s@%drows", tc.name, rows), func(t *testing.T) {
			m := testTUI(t)
			m.width, m.height = 80, rows
			m.loadServiceFields(0)
			m.fInfraTemplate, m.fMesh = tc.tmpl, tc.mesh
			m.screen, m.tabActive, m.editTab = screenServiceEdit, false, tabMetricsLogs
			m.form = m.makeServiceTabForm(tabMetricsLogs)
			m.form.Init()
			if got := strings.Count(m.View(), "\n") + 1; got > rows {
				t.Errorf("Metrics & logs renders %d rows, exceeding a %d-row terminal", got, rows)
			}
		})
	}
}

// TestInheritedMetricsNoteNamesTheSeries checks the note actually lists what
// the service emits beyond its configured metric, and escapes underscores for
// huh's markdown mini-renderer (system.cpu.load_average.1m would otherwise
// render as italics).
func TestInheritedMetricsNoteNamesTheSeries(t *testing.T) {
	note, lines := inheritedMetricsNote(Service{Name: "svc", InfraTemplate: "otel-host-process"}, 76, 0)
	if lines == 0 {
		t.Fatal("no note for a template that emits metrics")
	}
	for _, want := range []string{"system.cpu.utilization", "process.memory.usage", "otel:host", "otel:process"} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "load_average") {
		t.Error("underscore not escaped — huh renders it as italics")
	}
	if !strings.Contains(note, `load\_average`) {
		t.Errorf("expected escaped underscore in:\n%s", note)
	}

	// A template that contributes nothing must not draw an empty note box.
	if note, lines := inheritedMetricsNote(Service{Name: "svc", InfraTemplate: "k8s"}, 76, 0); note != "" || lines != 0 {
		t.Errorf("k8s should produce no note, got %d lines: %q", lines, note)
	}
}
