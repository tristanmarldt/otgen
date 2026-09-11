package main

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// ── messages ──────────────────────────────────────────────────────────────────

type tickMsg struct{}

type testResultMsg struct{ err error }

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func testConnCmd(app *App) tea.Cmd {
	return func() tea.Msg { return testResultMsg{err: app.TestConnection()} }
}

// ── screens ───────────────────────────────────────────────────────────────────

type tuiScreen int

const (
	screenList tuiScreen = iota
	screenServiceEdit
	screenGlobal
	screenConfirmDelete
	screenConfirmDiscard
	screenHelp
	screenPayload
)

const (
	tabService = iota
	tabSpans
	tabCalls
	tabInfrastructure
	tabMetricsLogs
	tabResourceAttrs
	tabSpanAttrs
)

var serviceTabNames = []string{"Service", "Spans", "Calls", "Infrastructure", "Metrics & logs", "Resource attrs", "Span attrs"}

// ── model ─────────────────────────────────────────────────────────────────────

// tui is the root Bubble Tea model. It uses a pointer receiver throughout so
// that huh form Value() pointers remain stable across Update calls.
type tui struct {
	app    *App
	width  int
	height int

	screen tuiScreen
	cursor int
	cfg    Config
	status RuntimeStatus

	form *huh.Form

	// service editor state
	editIdx   int     // index in cfg.Services; -1 = new service
	editTab   int     // active tab
	tabActive bool    // true = tab selector focused; false = huh form focused
	origSvc   Service // snapshot used for unsaved-change detection

	// bound form fields – service editor
	fName          string
	fTemplate      string
	fInfraCategory string // "" | "kubernetes" | "container" | "serverless" | "host" | "other"
	fInfraTemplate string
	fInfraStep     int // 0 = category select, 1 = template select, 2 = host/process name
	fHostName      string
	fProcessName   string
	fSpanKind      string
	fFailure       string
	fInterval      string
	fChildSpans    string
	fSignals       []string
	fDownstream    []string
	fMetricType    string
	fMetricName    string
	fMetricUnit    string
	fLogSeverity   string
	fMesh          bool
	fEnabled       bool
	fAttrs         string
	fSpanAttrs     string

	fDeleteConfirmed  bool
	fDiscardConfirmed bool

	// global config state
	gEndpoint string
	gToken    string
	gAttrs    string

	testing    bool
	spinnerIdx int // advances on each tick while testing == true

	flash    string
	flashErr bool
	flashEnd time.Time

	// payload preview (screenPayload)
	payloadSummary       string
	payloadJSON          string
	payloadMode          int // 0 = config summary, 1 = OTLP JSON
	payloadScroll        int
	payloadPrevScreen    tuiScreen
	payloadPrevTabActive bool
}

func NewTUIModel(app *App) *tui {
	return &tui{
		app:     app,
		cfg:     app.GetConfig(),
		status:  app.GetStatus(),
		editIdx: -1,
	}
}

// ── tea.Model ─────────────────────────────────────────────────────────────────

func (m *tui) Init() tea.Cmd {
	return tickCmd()
}

func (m *tui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		m.cfg = m.app.GetConfig()
		m.status = m.app.GetStatus()
		if m.testing {
			m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
		}
		if !m.flashEnd.IsZero() && time.Now().After(m.flashEnd) {
			m.flash = ""
			m.flashEnd = time.Time{}
		}
		return m, tickCmd()

	case testResultMsg:
		m.testing = false
		if msg.err != nil {
			m.setFlash("connection failed: "+msg.err.Error(), true)
		} else {
			m.setFlash("connection OK — endpoint accepted a test span", false)
		}
		return m, nil
	}

	switch m.screen {
	case screenHelp:
		if _, ok := msg.(tea.KeyMsg); ok {
			m.screen = screenList
		}
		return m, nil

	case screenPayload:
		if k, ok := msg.(tea.KeyMsg); ok {
			return m.updatePayload(k)
		}
		return m, nil

	case screenServiceEdit:
		if m.tabActive {
			return m.updateServiceSelector(msg)
		}
		return m.updateForm(msg)

	case screenGlobal:
		return m.updateForm(msg)

	case screenList:
		if k, ok := msg.(tea.KeyMsg); ok {
			return m.updateList(k)
		}
		return m, nil
	}

	return m.updateForm(msg)
}

// ── form delegation ───────────────────────────────────────────────────────────

func (m *tui) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.form == nil {
		m.screen = screenList
		return m, nil
	}

	if k, ok := msg.(tea.KeyMsg); ok {
		// Esc leaves the form. Inside a tabbed editor it returns to that
		// editor's selector (field values survive); elsewhere to the list.
		if k.Type == tea.KeyEsc {
			return m.leaveForm()
		}
	}

	newModel, cmd := m.form.Update(msg)
	if f, ok := newModel.(*huh.Form); ok {
		m.form = f
	}
	switch m.form.State {
	case huh.StateCompleted:
		return m.commitForm()
	case huh.StateAborted:
		return m.leaveForm()
	}
	return m, cmd
}

// leaveForm handles Esc / abort out of an open form.
func (m *tui) leaveForm() (tea.Model, tea.Cmd) {
	if m.screen == screenServiceEdit {
		if m.editTab == tabInfrastructure {
			// ESC walks back through infra steps: 2→1→0→tab selector.
			if m.fInfraStep == 2 {
				m.fInfraStep = 1
				m.form = m.makeServiceTabForm(tabInfrastructure)
				return m, m.form.Init()
			}
			if m.fInfraStep == 1 {
				m.fInfraStep = 0
				m.form = m.makeServiceTabForm(tabInfrastructure)
				return m, m.form.Init()
			}
		}
		m.fInfraStep = 0
		m.tabActive = true
	} else {
		m.screen = screenList
	}
	m.form = nil
	return m, nil
}

func (m *tui) commitForm() (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenServiceEdit:
		// Infrastructure tab is a multi-step flow: category → template → name(s).
		if m.editTab == tabInfrastructure {
			if m.fInfraStep == 0 {
				if m.fInfraCategory == "" {
					// "None" chosen — clear template and return.
					m.fInfraTemplate = ""
					m.fInfraStep = 0
					m.tabActive = true
					m.form = nil
					return m, nil
				}
				m.fInfraStep = 1
				m.form = m.makeServiceTabForm(tabInfrastructure)
				return m, m.form.Init()
			}
			if m.fInfraStep == 1 && infraUsesHostName(m.fInfraTemplate) {
				m.fInfraStep = 2
				m.form = m.makeServiceTabForm(tabInfrastructure)
				return m, m.form.Init()
			}
		}
		// All other tabs (and infra steps 1 without name step, or step 2): return to selector.
		m.fInfraStep = 0
		m.tabActive = true
		m.form = nil
		return m, nil
	case screenGlobal:
		return m.commitGlobal()
	case screenConfirmDelete:
		return m.commitDelete()
	case screenConfirmDiscard:
		return m.commitDiscard()
	}
	m.screen = screenList
	m.form = nil
	return m, nil
}

// ── list key handling ─────────────────────────────────────────────────────────

func (m *tui) updateList(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	svcs := m.cfg.Services
	switch k.String() {
	case "ctrl+c", "q":
		if m.status.Running {
			m.app.Stop()
		}
		return m, tea.Quit

	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}

	case "down", "j":
		if m.cursor < len(svcs)-1 {
			m.cursor++
		}

	case "n":
		m.loadServiceFields(-1)
		m.tabActive = false
		m.editTab = 0
		m.screen = screenServiceEdit
		m.form = m.makeServiceTabForm(0)
		return m, m.form.Init()

	case "enter":
		// Existing service: open the tab selector.
		if len(svcs) > 0 {
			m.loadServiceFields(m.cursor)
			m.tabActive = true
			m.editTab = 0
			m.form = nil
			m.screen = screenServiceEdit
			return m, nil
		}

	case "d":
		if len(svcs) > 0 {
			return m.openDeleteConfirm()
		}

	case " ":
		if len(svcs) > 0 {
			return m.toggleService()
		}

	case "r":
		return m.toggleRunning()

	case "t":
		if m.testing {
			return m, nil
		}
		m.testing = true
		m.spinnerIdx = 0
		return m, testConnCmd(m.app)

	case "ctrl+q":
		if len(svcs) > 0 {
			return m.openPayloadPreview()
		}

	case "g":
		m.gEndpoint = m.cfg.Endpoint
		m.gToken = m.cfg.Token
		m.gAttrs = attrsToText(m.cfg.Attributes)
		m.screen = screenGlobal
		m.form = m.makeGlobalForm()
		return m, m.form.Init()

	case "?":
		m.screen = screenHelp
		return m, nil
	}
	return m, nil
}

// ── service editor: field loading & template seeding ──────────────────────────

func (m *tui) loadServiceFields(idx int) {
	m.editIdx = idx
	if idx == -1 {
		m.fName = defaultServiceNamePrefix
		m.fTemplate = ""
		m.fInfraCategory = ""
		m.fInfraTemplate = ""
		m.fHostName = ""
		m.fProcessName = ""
		m.fSpanKind = "server"
		m.fFailure = "5"
		m.fInterval = "5"
		m.fChildSpans = "0"
		m.fSignals = []string{"logs", "metrics", "spans"}
		m.fDownstream = nil
		m.fMetricType = "sum"
		m.fMetricName = ""
		m.fMetricUnit = ""
		m.fLogSeverity = "info"
		m.fMesh = false
		m.fEnabled = true
		m.fAttrs = ""
		m.fSpanAttrs = ""
	} else {
		svc := m.cfg.Services[idx]
		m.fName = svc.Name
		m.fTemplate = svc.Template
		m.fInfraCategory = infraCategoryOf[svc.InfraTemplate]
		m.fInfraTemplate = svc.InfraTemplate
		m.fHostName = svc.HostName
		m.fProcessName = svc.ProcessName
		m.fSpanKind = svc.SpanKind
		m.fFailure = strconv.Itoa(svc.FailureRate)
		m.fInterval = strconv.Itoa(svc.Interval)
		m.fChildSpans = strconv.Itoa(svc.ChildSpans)
		if len(svc.Signals) == 0 {
			m.fSignals = []string{"logs", "metrics", "spans"}
		} else {
			m.fSignals = append([]string(nil), svc.Signals...)
			sort.Strings(m.fSignals)
		}
		m.fDownstream = append([]string(nil), svc.DownstreamCalls...)
		effectiveMetric := effectiveMetricConfig(svc)
		m.fMetricType = effectiveMetric.Type
		m.fMetricName = ""
		m.fMetricUnit = ""
		if svc.Metric != nil {
			m.fMetricName = svc.Metric.Name
			m.fMetricUnit = svc.Metric.Unit
		}
		m.fLogSeverity = effectiveLogSeverity(svc)
		m.fMesh = svc.Mesh
		m.fEnabled = svc.Enabled

		m.fAttrs = attrsToText(svc.Attributes)
		m.fSpanAttrs = attrsToText(svc.SpanAttrs)
	}
	m.origSvc = m.buildServiceFromFields()
}

// buildServiceFromFields materialises the editor state into a Service.
func (m *tui) buildServiceFromFields() Service {
	failRate, _ := strconv.Atoi(strings.TrimSpace(m.fFailure))
	interval, _ := strconv.Atoi(strings.TrimSpace(m.fInterval))
	childSpans, _ := strconv.Atoi(strings.TrimSpace(m.fChildSpans))

	signals := append([]string(nil), m.fSignals...)
	sort.Strings(signals)
	if len(signals) == 3 {
		signals = nil // all three = store empty (= all enabled)
	}
	metricConfig := MetricConfig{
		Type: strings.TrimSpace(m.fMetricType),
		Name: strings.TrimSpace(m.fMetricName),
		Unit: strings.TrimSpace(m.fMetricUnit),
	}
	var metric *MetricConfig
	if !(metricConfig.Type == "sum" && metricConfig.Name == "" && metricConfig.Unit == "") {
		metric = &metricConfig
	}
	severity := strings.TrimSpace(m.fLogSeverity)
	if severity == "info" {
		severity = ""
	}

	return normalizeService(Service{
		Name:            strings.TrimSpace(m.fName),
		Template:        m.fTemplate,
		InfraTemplate:   m.fInfraTemplate,
		HostName:        strings.TrimSpace(m.fHostName),
		ProcessName:     strings.TrimSpace(m.fProcessName),
		SpanKind:        m.fSpanKind,
		FailureRate:     failRate,
		Interval:        interval,
		ChildSpans:      childSpans,
		Signals:         signals,
		DownstreamCalls: append([]string(nil), m.fDownstream...),
		Metric:          metric,
		LogSeverity:     severity,
		Mesh:            m.fMesh,
		Enabled:         m.fEnabled,
		Attributes:      parseAttrs(m.fAttrs),
		SpanAttrs:       parseAttrs(m.fSpanAttrs),
	})
}

// hasUnsavedChanges reports whether the editor differs from the last saved state.
func (m *tui) hasUnsavedChanges() bool {
	return !reflect.DeepEqual(m.buildServiceFromFields(), m.origSvc)
}

// ── service editor: forms ─────────────────────────────────────────────────────

const attrTypeHint = "key=value · bool/number auto-detected · quote strings"
const defaultServiceNamePrefix = "otgen-"

// ── infrastructure template hierarchy ─────────────────────────────────────────

// infraCategoryOf maps each template ID to its logical category so that
// loadServiceFields can restore fInfraCategory from a saved InfraTemplate.
var infraCategoryOf = map[string]string{
	"k8s": "kubernetes", "eks": "kubernetes", "gke": "kubernetes",
	"aks": "kubernetes", "openshift": "kubernetes",
	"docker": "container", "containerd": "container",
	"ecs": "container", "azure-container-apps": "container",
	"lambda": "serverless", "azure-functions": "serverless", "gcp-functions": "serverless",
	"host": "host", "process": "host",
	"otel-host": "host", "otel-host-process": "host",
	"nomad": "other", "cloudfoundry": "other",
}

// infraTemplatesForCategory returns the Select options for a given category.
// An empty category ("None") returns a single placeholder so the value
// pointer is cleanly set to "" when the user picks None.
func infraTemplatesForCategory(cat string) []huh.Option[string] {
	switch cat {
	case "kubernetes":
		return []huh.Option[string]{
			huh.NewOption("Vanilla / generic", "k8s"),
			huh.NewOption("Amazon EKS", "eks"),
			huh.NewOption("Google GKE", "gke"),
			huh.NewOption("Azure AKS", "aks"),
			huh.NewOption("Red Hat OpenShift", "openshift"),
		}
	case "container":
		return []huh.Option[string]{
			huh.NewOption("Docker", "docker"),
			huh.NewOption("containerd", "containerd"),
			huh.NewOption("Amazon ECS / Fargate", "ecs"),
			huh.NewOption("Azure Container Apps", "azure-container-apps"),
		}
	case "serverless":
		return []huh.Option[string]{
			huh.NewOption("AWS Lambda", "lambda"),
			huh.NewOption("Azure Functions", "azure-functions"),
			huh.NewOption("Google Cloud Functions", "gcp-functions"),
		}
	case "host":
		return []huh.Option[string]{
			huh.NewOption("VM / bare metal", "host"),
			huh.NewOption("Process", "process"),
			huh.NewOption("OTel host", "otel-host"),
			huh.NewOption("OTel host + process", "otel-host-process"),
		}
	case "other":
		return []huh.Option[string]{
			huh.NewOption("HashiCorp Nomad", "nomad"),
			huh.NewOption("Cloud Foundry / Tanzu", "cloudfoundry"),
		}
	default: // "" = None
		return []huh.Option[string]{huh.NewOption("—", "")}
	}
}

func (m *tui) makeServiceTabForm(tabIdx int) *huh.Form {
	w := m.formWidth()
	metricPreview := effectiveMetricConfig(Service{
		Name:   strings.TrimSpace(m.fName),
		Metric: &MetricConfig{Type: m.fMetricType, Name: m.fMetricName, Unit: m.fMetricUnit},
	})
	switch tabIdx {
	case tabService:
		return huh.NewForm(
			huh.NewGroup(
				huh.NewInput().
					Title(settingsLabel("Service name")).
					Inline(true).
					Value(&m.fName).
					Validate(func(s string) error {
						if strings.TrimSpace(s) == "" {
							return fmt.Errorf("name is required")
						}
						return nil
					}),
				huh.NewInput().
					Title(settingsLabel("Interval (s)")).
					Inline(true).
					Value(&m.fInterval).
					Validate(func(s string) error {
						n, err := strconv.Atoi(strings.TrimSpace(s))
						if err != nil || n < 1 {
							return fmt.Errorf("must be ≥ 1")
						}
						return nil
					}),
				huh.NewInput().
					Title(settingsLabel("Failure rate %")).
					Inline(true).
					Value(&m.fFailure).
					Validate(func(s string) error {
						n, err := strconv.Atoi(strings.TrimSpace(s))
						if err != nil || n < 0 || n > 100 {
							return fmt.Errorf("must be a number 0–100")
						}
						return nil
					}),
				huh.NewMultiSelect[string]().
					Title(settingsLabel("Signals")).
					Options(
						huh.NewOption("spans", "spans"),
						huh.NewOption("metrics", "metrics"),
						huh.NewOption("logs", "logs"),
					).
					Value(&m.fSignals),
				huh.NewConfirm().
					Title(settingsLabel("Istio mesh")).
					Inline(true).
					Affirmative("on").
					Negative("off").
					Value(&m.fMesh),
			),
		).WithWidth(w)

	case tabSpans:
		return huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Span template").
					Description("OTel semantic-convention attributes on each span · / to filter").
					Options(
						huh.NewOption("None (generic)", ""),
						huh.NewOption("HTTP · server", "http-server"),
						huh.NewOption("HTTP · client", "http-client"),
						huh.NewOption("Database (db.*)", "db"),
						huh.NewOption("Messaging (Kafka / RabbitMQ / SQS)", "messaging"),
						huh.NewOption("gRPC", "grpc"),
					).
					Value(&m.fTemplate),
				huh.NewSelect[string]().
					Title(settingsLabel("Span kind")).
					Inline(true).
					Options(
						huh.NewOption("server", "server"),
						huh.NewOption("client", "client"),
						huh.NewOption("internal", "internal"),
						huh.NewOption("producer", "producer"),
						huh.NewOption("consumer", "consumer"),
					).
					Description("←/→ change").
					Value(&m.fSpanKind),
				huh.NewInput().
					Title(settingsLabel("Local child spans")).
					Inline(true).
					Value(&m.fChildSpans).
					Validate(func(s string) error {
						n, err := strconv.Atoi(strings.TrimSpace(s))
						if err != nil || n < 0 || n > 10 {
							return fmt.Errorf("must be 0–10")
						}
						return nil
					}),
			),
		).WithWidth(w)

	case tabCalls:
		var callOpts []huh.Option[string]
		for i, svc := range m.cfg.Services {
			if i != m.editIdx {
				callOpts = append(callOpts, huh.NewOption(svc.Name, svc.Name))
			}
		}
		if len(callOpts) == 0 {
			return huh.NewForm(
				huh.NewGroup(
					huh.NewNote().
						Title("Downstream calls").
						Description("No other services configured yet.\nAdd more services to set up call chains."),
				),
			).WithWidth(w)
		}
		return huh.NewForm(
			huh.NewGroup(
				huh.NewMultiSelect[string]().
					Title("Downstream calls").
					Description("Services this one calls · space to toggle").
					Options(callOpts...).
					Value(&m.fDownstream),
			),
		).WithWidth(w)

	case tabMetricsLogs:
		metricsNote, _ := inheritedMetricsNote(Service{
			Name:          strings.TrimSpace(m.fName),
			InfraTemplate: m.fInfraTemplate,
			Mesh:          m.fMesh,
		}, w-4, max(2, m.textLines()-8))
		metricFields := []huh.Field{
			huh.NewSelect[string]().
				Title(settingsLabel("Metric type")).
				Inline(true).
				Options(
					huh.NewOption("Sum", "sum"),
					huh.NewOption("Gauge", "gauge"),
					huh.NewOption("Histogram", "histogram"),
				).
				Description("Used when metrics are enabled").
				Value(&m.fMetricType),
			huh.NewInput().
				Title(settingsLabel("Metric name")).
				Inline(true).
				Placeholder(metricPreview.Name).
				Value(&m.fMetricName),
			huh.NewInput().
				Title(settingsLabel("Metric unit")).
				Inline(true).
				Placeholder(metricPreview.Unit).
				Value(&m.fMetricUnit),
			huh.NewSelect[string]().
				Title(settingsLabel("Log severity")).
				Inline(true).
				Options(
					huh.NewOption("DEBUG", "debug"),
					huh.NewOption("INFO", "info"),
					huh.NewOption("WARN", "warn"),
					huh.NewOption("ERROR", "error"),
				).
				Description("Used when logs are enabled").
				Value(&m.fLogSeverity),
		}
		// Read-only context goes last, so the editable fields stay at the top
		// of the tab where the cursor lands.
		//
		// On a short terminal it is dropped entirely: the four editable fields
		// already fill ~16 rows and a huh Note costs several more in chrome
		// alone, and huh cannot scroll a group that overflows. The tab summary
		// still shows the count, and ctrl+q still lists every series.
		if metricsNote != "" && m.textLines() >= 10 {
			metricFields = append(metricFields, huh.NewNote().
				Title("Also emitted (read-only)").
				Description(metricsNote))
		}
		return huh.NewForm(huh.NewGroup(metricFields...)).WithWidth(w)

	case tabInfrastructure:
		// Two-step flow: category first, then the template within that category.
		// Static Options() on both selects avoids huh v1's OptionsFunc viewport
		// bug (YOffset pinned to selected on every Update → text scrolls, cursor
		// stays put). commitForm() advances step 0→1; leaveForm() walks step 1→0.
		if m.fInfraStep == 0 {
			return huh.NewForm(
				huh.NewGroup(
					huh.NewSelect[string]().
						Title("Infrastructure — category").
						Description("Choose a deployment environment · enter to continue").
						Options(
							huh.NewOption("None", ""),
							huh.NewOption("Kubernetes", "kubernetes"),
							huh.NewOption("Container", "container"),
							huh.NewOption("Serverless", "serverless"),
							huh.NewOption("Host", "host"),
							huh.NewOption("Other", "other"),
						).
						Value(&m.fInfraCategory),
				),
			).WithWidth(w)
		}
		if m.fInfraStep == 1 {
			// step 1 — template within the chosen category
			return huh.NewForm(
				huh.NewGroup(
					huh.NewSelect[string]().
						Title("Infrastructure — template").
						Description("Specific environment variant · / to filter · esc back to category").
						Options(infraTemplatesForCategory(m.fInfraCategory)...).
						Value(&m.fInfraTemplate),
				),
			).WithWidth(w)
		}
		// step 2 — host name (and optionally process name)
		nameFields := []huh.Field{
			huh.NewInput().
				Title(settingsLabel("Host name")).
				Description("host.name · leave blank for default · esc back to template").
				Placeholder(effectiveHostName(Service{Name: strings.TrimSpace(m.fName), InfraTemplate: m.fInfraTemplate})).
				Value(&m.fHostName),
		}
		if infraUsesProcessName(m.fInfraTemplate) {
			nameFields = append(nameFields, huh.NewInput().
				Title(settingsLabel("Process name")).
				Description("process.executable.name · leave blank to use service name").
				Placeholder(strings.TrimSpace(m.fName)).
				Value(&m.fProcessName))
		}
		return huh.NewForm(huh.NewGroup(nameFields...)).WithWidth(w)

	case tabResourceAttrs:
		svcForNote := Service{
			Name:          strings.TrimSpace(m.fName),
			InfraTemplate: m.fInfraTemplate,
			HostName:      strings.TrimSpace(m.fHostName),
			ProcessName:   strings.TrimSpace(m.fProcessName),
			Mesh:          m.fMesh,
		}
		resNote, resNoteLines := inheritedResAttrsNote(m.cfg, svcForNote, w-4)
		var resFields []huh.Field
		if resNote != "" {
			resFields = append(resFields, huh.NewNote().
				Title("Inherited (read-only)").
				Description(resNote))
		}
		resFields = append(resFields, huh.NewText().
			Title("Resource attribute overrides").
			Description(attrTypeHint).
			Lines(max(1, m.textLines()-max(0, resNoteLines+3))).
			Value(&m.fAttrs))
		return huh.NewForm(huh.NewGroup(resFields...)).WithWidth(w)

	default: // tabSpanAttrs
		spanNote, spanNoteLines := inheritedSpanAttrsNote(m.fTemplate, w-4)
		var spanFields []huh.Field
		if spanNote != "" {
			spanFields = append(spanFields, huh.NewNote().
				Title("From template (read-only)").
				Description(spanNote))
		}
		spanFields = append(spanFields, huh.NewText().
			Title("Span attribute overrides").
			Description(attrTypeHint).
			Lines(max(1, m.textLines()-max(0, spanNoteLines+3))).
			Value(&m.fSpanAttrs))
		return huh.NewForm(huh.NewGroup(spanFields...)).WithWidth(w)
	}
}

// settingsLabel pads inline field titles so the values line up.
func settingsLabel(s string) string {
	return fmt.Sprintf("%-22s", s)
}

// resetInfraStepIfNeeded resets the infrastructure two-step flow to step 0
// and re-derives fInfraCategory from the actual saved template whenever the
// infra tab is opened from the tab selector, so stale UI state is discarded.
func (m *tui) resetInfraStepIfNeeded() {
	if m.editTab == tabInfrastructure {
		m.fInfraStep = 0
		m.fInfraCategory = infraCategoryOf[m.fInfraTemplate]
	}
}

func (m *tui) commitService() (tea.Model, tea.Cmd) {
	svc := m.buildServiceFromFields()

	cfg := m.cfg
	services := append([]Service(nil), cfg.Services...)
	oldName := ""
	if m.editIdx >= 0 && m.editIdx < len(services) {
		oldName = services[m.editIdx].Name
	}
	if m.editIdx == -1 {
		services = append(services, svc)
		m.editIdx = len(services) - 1
	} else {
		services[m.editIdx] = svc
	}
	cfg.Services = services
	if oldName != "" && oldName != svc.Name {
		renameServiceReferences(&cfg, oldName, svc.Name)
	}
	cfg = normalizeConfig(cfg)
	if err := validateConfig(cfg); err != nil {
		m.setFlash("error: "+err.Error(), true)
		m.tabActive = true
		m.form = nil
		return m, nil
	}

	if err := m.app.SetConfig(cfg); err != nil {
		m.setFlash("error: "+err.Error(), true)
		m.tabActive = true
		m.form = nil
		return m, nil
	}

	m.cfg = cfg
	m.origSvc = svc
	m.setFlash("saved "+svc.Name, false)

	m.screen = screenList
	m.form = nil
	m.tabActive = false
	return m, nil
}

// ── service editor: tab selector ──────────────────────────────────────────────

func (m *tui) updateServiceSelector(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch k.String() {
	case "esc", "q":
		if m.hasUnsavedChanges() {
			return m.openDiscardConfirm()
		}
		m.screen = screenList
		m.tabActive = false
		return m, nil

	case "s":
		return m.commitService()

	case "up", "k", "left", "h", "[":
		if m.editTab > 0 {
			m.editTab--
		}

	case "down", "j", "right", "l", "]":
		if m.editTab < len(serviceTabNames)-1 {
			m.editTab++
		}

	case "ctrl+q":
		return m.openPayloadPreview()

	case "1", "2", "3", "4", "5", "6", "7":
		m.editTab = int(k.Runes[0] - '1')
		m.resetInfraStepIfNeeded()
		m.tabActive = false
		m.form = m.makeServiceTabForm(m.editTab)
		return m, m.form.Init()

	case "enter", " ":
		m.resetInfraStepIfNeeded()
		m.tabActive = false
		m.form = m.makeServiceTabForm(m.editTab)
		return m, m.form.Init()
	}
	return m, nil
}

// ── global config ─────────────────────────────────────────────────────────────

func (m *tui) makeGlobalForm() *huh.Form {
	w := m.formWidth()
	endpointDesc := "e.g. https://xxx.live.dynatrace.com/api/v2/otlp"
	if endpointFromEnv() {
		endpointDesc = "⚠ OTGEN_ENDPOINT is set — it overrides this value at runtime"
	}
	tokenDesc := "Edit or clear the saved token"
	if !m.cfg.hasToken() {
		tokenDesc = "Optional — leave blank if unused"
	}
	if tokenFromEnv() {
		tokenDesc = "⚠ OTGEN_TOKEN is set — it overrides this value at runtime"
	}

	return huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("OTLP endpoint").
				Description(endpointDesc).
				Value(&m.gEndpoint),
			huh.NewInput().
				Title("API token").
				Description(tokenDesc).
				Password(true).
				Value(&m.gToken),
			huh.NewText().
				Title("Global resource attributes").
				Description("Lowest precedence · "+attrTypeHint).
				Lines(max(5, m.textLines()-6)).
				Value(&m.gAttrs),
		),
	).WithWidth(w)
}

func (m *tui) commitGlobal() (tea.Model, tea.Cmd) {
	cfg := m.cfg
	cfg.Endpoint = strings.TrimSpace(m.gEndpoint)
	cfg.Attributes = parseAttrs(m.gAttrs)
	cfg.Token = strings.TrimSpace(m.gToken)

	if err := m.app.SetConfig(cfg); err != nil {
		m.setFlash("error: "+err.Error(), true)
	} else {
		m.cfg = m.app.GetConfig()
		m.setFlash("settings saved", false)
	}
	m.screen = screenList
	m.form = nil
	return m, nil
}

// ── confirmations ─────────────────────────────────────────────────────────────

func (m *tui) openDeleteConfirm() (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.cfg.Services) {
		return m, nil
	}
	svcName := m.cfg.Services[m.cursor].Name
	m.fDeleteConfirmed = false
	m.form = huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("Delete %q?", svcName)).
				Affirmative("Yes, delete").
				Negative("Cancel").
				Value(&m.fDeleteConfirmed),
		),
	).WithWidth(m.formWidth())
	m.screen = screenConfirmDelete
	return m, m.form.Init()
}

func (m *tui) commitDelete() (tea.Model, tea.Cmd) {
	m.screen = screenList
	m.form = nil
	if !m.fDeleteConfirmed {
		return m, nil
	}

	cfg := m.cfg
	if m.cursor < len(cfg.Services) {
		name := cfg.Services[m.cursor].Name
		if referrers := serviceReferrers(cfg, name); len(referrers) > 0 {
			m.setFlash(fmt.Sprintf("cannot delete %s; referenced by %s", name, strings.Join(referrers, ", ")), true)
			return m, nil
		}
		services := append([]Service(nil), cfg.Services...)
		services = append(services[:m.cursor], services[m.cursor+1:]...)
		cfg.Services = services
		if m.cursor >= len(cfg.Services) && m.cursor > 0 {
			m.cursor--
		}
		if err := m.app.SetConfig(cfg); err != nil {
			m.setFlash("error: "+err.Error(), true)
		} else {
			m.cfg = cfg
			m.setFlash("deleted "+name, false)
		}
	}
	return m, nil
}

func (m *tui) openDiscardConfirm() (tea.Model, tea.Cmd) {
	m.fDiscardConfirmed = false
	m.form = huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Discard unsaved changes to " + strings.TrimSpace(m.fName) + "?").
				Description("Press s in the tab list to save instead.").
				Affirmative("Yes, discard").
				Negative("Keep editing").
				Value(&m.fDiscardConfirmed),
		),
	).WithWidth(m.formWidth())
	m.screen = screenConfirmDiscard
	m.tabActive = false
	return m, m.form.Init()
}

func (m *tui) commitDiscard() (tea.Model, tea.Cmd) {
	m.form = nil
	if m.fDiscardConfirmed {
		m.screen = screenList
		m.tabActive = false
		return m, nil
	}
	// keep editing — back to the tab selector
	m.screen = screenServiceEdit
	m.tabActive = true
	return m, nil
}

// ── toggles ───────────────────────────────────────────────────────────────────

func (m *tui) toggleService() (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.cfg.Services) {
		return m, nil
	}
	svcs := append([]Service(nil), m.cfg.Services...)
	svcs[m.cursor].Enabled = !svcs[m.cursor].Enabled

	cfg := m.cfg
	cfg.Services = svcs
	if err := m.app.SetConfig(cfg); err != nil {
		m.setFlash("error: "+err.Error(), true)
	} else {
		m.cfg = cfg
	}
	return m, nil
}

func (m *tui) toggleRunning() (tea.Model, tea.Cmd) {
	if m.status.Running {
		m.app.Stop()
		m.setFlash("stopped", false)
	} else {
		if err := m.app.Start(); err != nil {
			m.setFlash("error: "+err.Error(), true)
		} else {
			m.setFlash("started", false)
		}
	}
	m.status = m.app.GetStatus()
	return m, nil
}

// ── flash ─────────────────────────────────────────────────────────────────────

func (m *tui) setFlash(msg string, isErr bool) {
	m.flash = msg
	m.flashErr = isErr
	m.flashEnd = time.Now().Add(4 * time.Second)
}

// ── view ──────────────────────────────────────────────────────────────────────

func (m *tui) View() string {
	if m.width == 0 {
		return "Loading…"
	}
	switch m.screen {
	case screenHelp:
		return m.helpView()

	case screenPayload:
		return m.payloadView()

	case screenServiceEdit:
		if m.tabActive {
			return m.serviceSelectorView()
		}
		if m.form != nil {
			return m.tabBar(m.editTab) + "\n" + sHelp.Render("  esc tab selector  ·  s save") + "\n" + m.form.View()
		}

	case screenGlobal:
		if m.form != nil {
			return "  " + sBold.Render("Global configuration") + "\n" + sHelp.Render("  esc cancel · enter save") + "\n" + m.form.View()
		}

	}
	if m.form != nil && m.screen != screenList {
		return m.form.View()
	}
	return m.listView()
}

// sepLine renders the single horizontal rule used across all screens.
func (m *tui) sepLine() string {
	w := m.width - 4
	if w > 72 {
		w = 72
	}
	if w < 20 {
		w = 20
	}
	return sMuted.Render(strings.Repeat("─", w))
}

// tabBar renders the compact bar shown above an open form. It is also the top
// of each selector view, so the two screens read as one continuous surface.
func (m *tui) tabBar(active int) string {
	// Full bar: every tab named. Falls back to numbers, then to the active tab
	// alone, so the bar never wraps on a narrow terminal.
	var full, numbered []string
	for i, name := range serviceTabNames {
		label := fmt.Sprintf("%d %s", i+1, name)
		num := strconv.Itoa(i + 1)
		if i == active {
			full = append(full, sTabActive.Render(label))
			numbered = append(numbered, sTabActive.Render(label))
		} else {
			full = append(full, sTabInactive.Render(label))
			numbered = append(numbered, sMuted.Render(num))
		}
	}

	sep := sMuted.Render(" │ ")
	for _, parts := range [][]string{full, numbered} {
		bar := "  " + strings.Join(parts, sep)
		if lipgloss.Width(bar) <= m.width {
			return bar + "\n  " + m.sepLine()
		}
	}
	bar := fmt.Sprintf("  %s  %s",
		sMuted.Render(fmt.Sprintf("tab %d/%d", active+1, len(serviceTabNames))),
		sTabActive.Render(serviceTabNames[active]))
	return bar + "\n  " + m.sepLine()
}

// serviceSelectorView lists the editor tabs with a summary of each one.
func (m *tui) serviceSelectorView() string {
	summaries := m.serviceTabSummaries()

	var rows []string
	rows = append(rows, m.tabBar(m.editTab))

	name := strings.TrimSpace(m.fName)
	if name == "" {
		name = "new service"
	}
	head := "  " + sBold.Render(name)
	if m.editIdx == -1 {
		head += sMuted.Render("  (not saved yet)")
	} else if m.hasUnsavedChanges() {
		head += "  " + sWarn.Render("● unsaved changes")
	} else {
		head += "  " + sMuted.Render("✓ saved")
	}
	rows = append(rows, head, "")

	for i, tabName := range serviceTabNames {
		line := fmt.Sprintf("%d %s", i+1, tabName)
		pad := 20 - len([]rune(line))
		if pad < 1 {
			pad = 1
		}
		line += strings.Repeat(" ", pad) + summaries[i]
		if i == m.editTab {
			rows = append(rows, sPrimary.Render("  ▸ ")+sPrimaryBold.Render(line))
		} else {
			rows = append(rows, sMuted.Render("    "+line))
		}
	}

	rows = append(rows, "", "  "+m.sepLine())
	rows = append(rows, sHelp.Render("  ↑↓/[]/1-7 navigate  ·  enter open  ·  s save  ·  ctrl+q preview  ·  esc back"))
	rows = append(rows, sHelp.Render("  ~ inherited from template   ✎ your override"))
	return strings.Join(rows, "\n")
}

// serviceTabSummaries describes the current contents of each editor tab.
func (m *tui) serviceTabSummaries() []string {
	sigs := "all signals"
	if len(m.fSignals) != 3 && len(m.fSignals) > 0 {
		s := append([]string(nil), m.fSignals...)
		sort.Strings(s)
		sigs = strings.Join(s, ",")
	} else if len(m.fSignals) == 0 {
		sigs = "no signals"
	}
	service := fmt.Sprintf("every %ss · %s%% err · %s",
		strings.TrimSpace(m.fInterval), strings.TrimSpace(m.fFailure), sigs)
	if m.fMesh {
		service += " · istio mesh"
	}
	spans := m.fTemplate
	if spans == "" {
		spans = sMuted.Render("none — generic spans")
	}
	spans += " · " + m.fSpanKind
	if n, _ := strconv.Atoi(strings.TrimSpace(m.fChildSpans)); n > 0 {
		spans += fmt.Sprintf(" · +%d local child", n)
	}
	calls := "none"
	if len(m.fDownstream) > 0 {
		calls = truncate(strings.Join(m.fDownstream, ", "), max(12, m.width-32))
	}
	metric := effectiveMetricConfig(Service{Name: strings.TrimSpace(m.fName), Metric: &MetricConfig{
		Type: m.fMetricType, Name: m.fMetricName, Unit: m.fMetricUnit,
	}})
	metricsLogs := fmt.Sprintf("%s %s · %s logs", metric.Type, metric.Name, strings.ToUpper(effectiveLogSeverity(Service{LogSeverity: m.fLogSeverity})))
	// The infra template can add system.*/process.* metrics of its own; without
	// this the extra series are invisible until you open the payload preview.
	if n := len(infraMetricNames(m.fInfraTemplate)); n > 0 {
		metricsLogs += fmt.Sprintf(" · +%d %s", n, m.fInfraTemplate)
	}
	infraTmpl := m.fInfraTemplate
	if infraTmpl == "" {
		infraTmpl = sMuted.Render("none")
	} else if infraUsesHostName(m.fInfraTemplate) {
		// These names are the Dynatrace entity identity, so show them here
		// rather than making the user open the tab to find out.
		svcForNames := Service{
			Name:          strings.TrimSpace(m.fName),
			InfraTemplate: m.fInfraTemplate,
			HostName:      strings.TrimSpace(m.fHostName),
			ProcessName:   strings.TrimSpace(m.fProcessName),
		}
		infraTmpl += " · " + effectiveHostName(svcForNames)
		if infraUsesProcessName(m.fInfraTemplate) {
			infraTmpl += "/" + effectiveProcessName(svcForNames)
		}
	}

	return []string{
		service,
		spans,
		calls,
		infraTmpl,
		metricsLogs,
		attrSummary(m.fAttrs, len(infraDefaults(Service{Name: strings.TrimSpace(m.fName), InfraTemplate: m.fInfraTemplate}))),
		attrSummary(m.fSpanAttrs, len(templateDefaults(m.fTemplate))),
	}
}

func attrSummary(text string, inherited int) string {
	n := len(parseAttrs(text))
	if n > 0 {
		return fmt.Sprintf("✎ %d override", n)
	}
	if inherited > 0 {
		return fmt.Sprintf("~ %d inherited", inherited)
	}
	return sMuted.Render("none")
}

func (m *tui) helpView() string {
	rows := []string{
		"  " + sPrimaryBold.Render("otgen") + sMuted.Render("  keyboard reference"),
		"  " + m.sepLine(),
		"",
		"  " + sBold.Render("Service list"),
		sHelp.Render("    ↑↓/jk move · enter edit · n new"),
		sHelp.Render("    space enable · d delete"),
		sHelp.Render("    r run · t test · g config · ctrl+q preview · q quit"),
		"",
		"  " + sBold.Render("Editor (tab selector)"),
		sHelp.Render("    enter/1-7/]/[ open/navigate · s save · esc back"),
		sHelp.Render("    ctrl+q payload preview"),
		sHelp.Render("    / filter templates · alt+enter new line in text fields"),
		"",
		"  " + sBold.Render("Attributes"),
		sHelp.Render("    ~ inherited · ✎ service override"),
		"",
		"  " + m.sepLine(),
		sHelp.Render("  press any key to go back"),
	}
	return strings.Join(rows, "\n")
}

func (m *tui) listView() string {
	var rows []string

	rows = append(rows, m.renderHeader())
	rows = append(rows, "")

	if len(m.cfg.Services) == 0 {
		rows = append(rows, sMuted.Render("  No services yet — press n to create one."))
	} else {
		for i, svc := range m.cfg.Services {
			rows = append(rows, m.renderService(svc, i == m.cursor))
		}
	}

	if m.flash != "" {
		rows = append(rows, "")
		msg := truncate(m.flash, max(20, m.width-6))
		if m.flashErr {
			rows = append(rows, sError.Render("  ✗ "+msg))
		} else {
			rows = append(rows, sSuccess.Render("  ✓ "+msg))
		}
	}

	body := strings.Join(rows, "\n")

	// pad so the help line sits at the bottom of the terminal
	lineCount := strings.Count(body, "\n") + 1
	targetLine := m.height - 2
	if lineCount < targetLine {
		body += strings.Repeat("\n", targetLine-lineCount)
	}
	return body + "\n" + m.renderHelp()
}

func (m *tui) renderHeader() string {
	indicator := sMuted.Render("○ stopped")
	if m.testing {
		indicator = sPrimary.Render(spinnerFrames[m.spinnerIdx] + " testing…")
	} else if m.status.Running {
		indicator = sSuccess.Render("● running")
	}

	ver := " v" + version
	if version == "dev" {
		ver = " (dev build)"
	}
	line1 := "  " + sPrimaryBold.Render("otgen") + sMuted.Render(ver) + "  " + indicator

	ep := m.cfg.Endpoint
	switch {
	case endpointFromEnv():
		ep = sMuted.Render(m.cfg.runtimeConfig().Endpoint) + " " + sWarn.Render("[env]")
	case ep == "":
		ep = sMuted.Italic(true).Render("no endpoint — press g to configure")
	default:
		ep = sMuted.Render(ep)
	}

	var badges []string
	if !m.cfg.hasToken() && !tokenFromEnv() {
		badges = append(badges, sWarn.Render("no token"))
	} else if tokenFromEnv() {
		badges = append(badges, sWarn.Render("token [env]"))
	}
	if n := len(m.cfg.Attributes); n > 0 {
		badges = append(badges, sMuted.Render(fmt.Sprintf("%d global attr", n)))
	}
	line2 := "  " + ep
	if len(badges) > 0 {
		line2 += sMuted.Render("  ·  ") + strings.Join(badges, sMuted.Render("  ·  "))
	}

	return line1 + "\n" + line2 + "\n  " + m.sepLine()
}

// renderService draws one service. The cursored row is expanded with its
// effective resource and span attributes; the others stay compact.
func (m *tui) renderService(svc Service, expanded bool) string {
	cursor := "  "
	style := colorForService(svc.Name)
	name := style.Render(svc.Name)
	if expanded {
		cursor = sPrimary.Render("▶ ")
		name = style.Bold(true).Render(svc.Name)
	}

	dot := sMuted.Render("○")
	if svc.Enabled {
		dot = sSuccess.Render("●")
	}

	meta := []string{svc.SpanKind}
	if svc.Template != "" {
		meta = append(meta, "["+svc.Template+"]")
	}
	if svc.InfraTemplate != "" {
		meta = append(meta, "["+svc.InfraTemplate+"]")
	}
	meta = append(meta, fmt.Sprintf("%ds", svc.Interval), fmt.Sprintf("%d%% err", svc.FailureRate))
	if svc.ChildSpans > 0 {
		meta = append(meta, fmt.Sprintf("+%d local child", svc.ChildSpans))
	}
	row1 := cursor + dot + " " + name + "  " + sMuted.Render(strings.Join(meta, "  "))

	var lines []string
	lines = append(lines, row1)

	// Second line: live counters, or a clear disabled/idle marker.
	switch {
	case !svc.Enabled:
		lines = append(lines, "    "+sMuted.Render("disabled — press space to enable"))
	case !m.status.Running:
		lines = append(lines, "    "+sMuted.Render("idle — press r to start sending"))
	default:
		ss := m.status.Services[svc.Name]
		var parts []string
		if svc.hasSignal(signalSpans) {
			parts = append(parts, fmt.Sprintf("spans↑%d", ss.Spans.SentCount))
		}
		if svc.hasSignal(signalMetrics) {
			parts = append(parts, fmt.Sprintf("metrics↑%d", ss.Metrics.SentCount))
		}
		if svc.hasSignal(signalLogs) {
			parts = append(parts, fmt.Sprintf("logs↑%d", ss.Logs.SentCount))
		}
		line := "    " + sMuted.Render(strings.Join(parts, "  "))
		for _, s := range []SignalStatus{ss.Spans, ss.Metrics, ss.Logs} {
			if s.LastError != "" {
				budget := m.width - lipgloss.Width(line) - 8
				line += "  " + sError.Render("! "+truncate(s.LastError, max(20, budget)))
				break
			}
		}
		lines = append(lines, line)
	}

	if !expanded {
		return strings.Join(lines, "\n")
	}

	// Expanded: show the attributes that will actually be emitted.
	resAttrs := make(map[string]AttrValue, len(m.cfg.Attributes)+len(svc.Attributes)+8)
	mergeAttrs(resAttrs, m.cfg.Attributes)
	mergeAttrs(resAttrs, infraDefaults(svc))
	mergeAttrs(resAttrs, svc.Attributes)
	resMark := "✎"
	if len(svc.Attributes) == 0 {
		resMark = "~"
	}
	lines = append(lines, m.attrLine("res ", resMark, resAttrs))

	spanAttrs := map[string]AttrValue{}
	mergeAttrs(spanAttrs, templateDefaults(svc.Template))
	mergeAttrs(spanAttrs, svc.SpanAttrs)
	spanMark := "✎"
	if len(svc.SpanAttrs) == 0 {
		spanMark = "~"
	}
	lines = append(lines, m.attrLine("span", spanMark, spanAttrs))

	return strings.Join(lines, "\n")
}

// attrLine renders one "res"/"span" preview row, fitted to the terminal width.
func (m *tui) attrLine(label, mark string, attrs map[string]AttrValue) string {
	if len(attrs) == 0 {
		return "    " + sMuted.Render(label+"  "+sMuted.Render("—"))
	}
	budget := m.width - 14
	if budget < 20 {
		budget = 20
	}
	preview, shown := attrsPreview(attrs, budget)
	if rest := len(attrs) - shown; rest > 0 {
		preview += fmt.Sprintf("  +%d", rest)
	}
	return "    " + sMuted.Render(label+" "+mark+" "+preview)
}

// renderHint formats a single help entry: the key letter(s) in accent bold,
// the description in muted — e.g. "n new" → bold-sky "n" + muted " new".
func renderHint(hint string) string {
	i := strings.IndexByte(hint, ' ')
	if i < 0 {
		return sHelpKey.Render(hint)
	}
	return sHelpKey.Render(hint[:i]) + sHelp.Render(hint[i:])
}

func (m *tui) renderHelp() string {
	full := []string{
		"n new", "↵ edit", "d delete", "␣ toggle",
		"r run/stop", "t test", "g config", "ctrl+q preview", "? help", "q quit",
	}
	medium := []string{"n new", "↵ edit", "r run/stop", "g config", "? help", "q quit"}
	short := []string{"↵ edit", "r run", "? help", "q quit"}

	sep := sHelp.Render("  ·  ")
	for _, set := range [][]string{full, medium, short} {
		parts := make([]string, len(set))
		for i, h := range set {
			parts[i] = renderHint(h)
		}
		line := "  " + strings.Join(parts, sep)
		if lipgloss.Width(line) <= m.width {
			return line
		}
	}
	return renderHint("? help")
}

// textLines sizes an attribute textarea to the terminal height.
func (m *tui) textLines() int {
	n := m.height - 12
	if n > 16 {
		n = 16
	}
	if n < 5 {
		n = 5
	}
	return n
}

func (m *tui) formWidth() int {
	w := m.width - 4
	if w > 80 {
		w = 80
	}
	if w < 40 {
		w = 40
	}
	return w
}

// ── attribute text helpers ────────────────────────────────────────────────────

// attrsPreview returns as many "key=value" pairs as fit in budget columns,
// along with how many were shown.
func attrsPreview(attrs map[string]AttrValue, budget int) (string, int) {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	used := 0
	for _, k := range keys {
		pair := k + "=" + attrValueText(attrs[k], false)
		if used > 0 && used+len(pair)+2 > budget {
			break
		}
		used += len(pair) + 2
		parts = append(parts, pair)
	}
	return strings.Join(parts, "  "), len(parts)
}

// attrValueText renders an AttrValue. When quote is true, strings that would
// be re-parsed as another type are wrapped in "" so they round-trip.
func attrValueText(v AttrValue, quote bool) string {
	switch v.Type {
	case "bool":
		if v.Bool {
			return "true"
		}
		return "false"
	case "int":
		return strconv.FormatInt(v.Int, 10)
	case "double":
		s := strconv.FormatFloat(v.Double, 'f', -1, 64)
		if quote && !strings.Contains(s, ".") {
			s += ".0" // ensure it round-trips as double, not int
		}
		return s
	default:
		if quote && attrStrNeedsQuoting(v.Str) {
			return `"` + v.Str + `"`
		}
		return v.Str
	}
}

// inheritedResAttrsNote returns a multi-line description of the resource
// attributes inherited by svc from global config, infra template, and Istio
// mesh. All attributes are shown (no truncation). Returns ("", 0) when nothing
// is inherited.
func inheritedResAttrsNote(cfg Config, svc Service, budget int) (string, int) {
	type src struct {
		label string
		attrs map[string]AttrValue
	}
	var sources []src
	if len(cfg.Attributes) > 0 {
		sources = append(sources, src{"global", cfg.Attributes})
	}
	if infra := infraDefaults(svc); len(infra) > 0 {
		sources = append(sources, src{svc.InfraTemplate, infra})
	}

	if len(sources) == 0 && svc.Name == "" {
		return "", 0
	}
	var lines []string
	for _, s := range sources {
		lines = append(lines, attrsBlock(s.label, s.attrs, budget)...)
	}
	if svc.Name != "" {
		lines = append(lines, "service.name="+noteEscape(svc.Name)+" (always set)")
	}
	return strings.Join(lines, "\n"), len(lines)
}

// inheritedMetricsNote lists the metrics a service emits in addition to the one
// configured on this tab: those the infra template contributes for Dynatrace
// entity extraction, and the Istio mesh series. Both are otherwise invisible
// until something fails to show up in Grail.
//
// Returns ("", 0) when the service emits nothing beyond its configured metric.
func inheritedMetricsNote(svc Service, budget, maxLines int) (string, int) {
	var lines []string

	if names := infraMetricNames(svc.InfraTemplate); len(names) > 0 {
		lines = append(lines, namesBlock(svc.InfraTemplate+" — creates "+entityKindsFor(svc.InfraTemplate), names, budget)...)
	}
	if svc.Mesh {
		lines = append(lines, namesBlock("istio mesh", istioMetricNames, budget)...)
	}
	if len(lines) == 0 {
		return "", 0
	}
	// huh cannot scroll a group that overflows its terminal, so trim rather
	// than push the fields off screen. The marker gets a line of its own:
	// appending it to the last content line makes that line re-wrap, which
	// costs back the very row the trim was meant to save.
	if maxLines > 0 && len(lines) > maxLines {
		keep := maxLines - 1
		if keep < 1 {
			keep = 1
		}
		hidden := len(lines) - keep
		lines = append(lines[:keep], fmt.Sprintf("  … +%d more (ctrl+q)", hidden))
	}
	return strings.Join(lines, "\n"), len(lines)
}

// entityKindsFor names the Dynatrace entities an infra template's metrics
// create, so the note explains why the extra series are there.
func entityKindsFor(template string) string {
	switch template {
	case "otel-host":
		return "otel:host"
	case "otel-host-process":
		return "otel:host + otel:process"
	}
	return "entities"
}

// namesBlock renders a labelled, width-wrapped list of metric names, matching
// how attrsBlock lays out inherited attributes.
func namesBlock(label string, names []string, lineWidth int) []string {
	header := fmt.Sprintf("%s (%d)", noteEscape(label), len(names))
	if len(names) == 0 {
		return []string{header}
	}
	width := lineWidth - 2 // 2-char indent on wrapped lines
	if width < 20 {
		width = 20
	}
	var out, cur []string
	used := 0
	for _, n := range names {
		item := noteEscape(n)
		add := len(item)
		if len(cur) > 0 {
			add += 2
		}
		if len(cur) > 0 && used+add > width {
			out = append(out, "  "+strings.Join(cur, "  "))
			cur, used, add = nil, 0, len(item)
		}
		cur = append(cur, item)
		used += add
	}
	if len(cur) > 0 {
		out = append(out, "  "+strings.Join(cur, "  "))
	}
	return append([]string{header}, out...)
}

// inheritedSpanAttrsNote returns a multi-line description of the span
// attributes provided by the selected template. All attributes are shown.
// Returns ("", 0) for no template.
func inheritedSpanAttrsNote(template string, budget int) (string, int) {
	tmpl := templateDefaults(template)
	if len(tmpl) == 0 {
		return "", 0
	}
	lines := attrsBlock(template, tmpl, budget)
	return strings.Join(lines, "\n"), len(lines)
}

// noteEscape escapes characters that huh's Note.Description mini-renderer
// treats as markdown: _ (italic), * (bold), ` (code). A leading \ causes
// the renderer to emit the next rune literally, so \_  →  _.
// Must be applied to any user-visible string passed to huh.NewNote().Description().
func noteEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "_", `\_`)
	s = strings.ReplaceAll(s, "*", `\*`)
	s = strings.ReplaceAll(s, "`", "\\`")
	return s
}

// attrsBlock returns a header line ("label (N)") followed by wrapped lines of
// "key=val" pairs. Every attribute is shown; pairs wrap when the next one
// would exceed lineWidth characters.
// Keys and values are noteEscape-d so that underscores survive huh's renderer.
func attrsBlock(label string, attrs map[string]AttrValue, lineWidth int) []string {
	header := fmt.Sprintf("%s (%d)", noteEscape(label), len(attrs))
	if len(attrs) == 0 {
		return []string{header}
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	budget := lineWidth - 2 // 2-char indent on each wrapped line
	if budget < 20 {
		budget = 20
	}
	var result []string
	var cur []string
	used := 0
	for _, k := range keys {
		pair := noteEscape(k) + "=" + noteEscape(attrValueText(attrs[k], false))
		addLen := len(pair)
		if len(cur) > 0 {
			addLen += 2 // "  " separator
		}
		if len(cur) > 0 && used+addLen > budget {
			result = append(result, "  "+strings.Join(cur, "  "))
			cur = nil
			used = 0
			addLen = len(pair)
		}
		cur = append(cur, pair)
		used += addLen
	}
	if len(cur) > 0 {
		result = append(result, "  "+strings.Join(cur, "  "))
	}
	return append([]string{header}, result...)
}

// attrsToText serialises a map[string]AttrValue to a human-editable
// "key=value" text (one entry per line, sorted by key).
func attrsToText(attrs map[string]AttrValue) string {
	if len(attrs) == 0 {
		return ""
	}
	lines := make([]string, 0, len(attrs))
	for k, v := range attrs {
		lines = append(lines, k+"="+attrValueText(v, true))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// attrStrNeedsQuoting reports whether a string value must be wrapped in ""
// so that parseAttrs can distinguish it from bool/int/double.
func attrStrNeedsQuoting(s string) bool {
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	if lower == "true" || lower == "false" {
		return true
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	return false
}

// parseAttrs parses "key=value" lines back into a typed AttrValue map.
//
// Type detection order (per value):
//  1. true / false (case-insensitive) → bool
//  2. "…" (double-quoted) → string (quotes stripped)
//  3. All digits with optional leading - → int64
//  4. Digits + decimal point → double
//  5. Everything else → string
func parseAttrs(text string) map[string]AttrValue {
	attrs := make(map[string]AttrValue)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		if k == "" {
			continue
		}
		attrs[k] = parseAttrValue(v)
	}
	return attrs
}

func parseAttrValue(v string) AttrValue {
	// 1. Boolean
	switch strings.ToLower(v) {
	case "true":
		return boolAttrVal(true)
	case "false":
		return boolAttrVal(false)
	}
	// 2. Quoted string – strip the surrounding quotes.
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return strAttrVal(v[1 : len(v)-1])
	}
	// 3 & 4. Number. Keep the first-digit check so values such as +1 and .5
	// retain their established string interpretation.
	numeric := strings.TrimPrefix(v, "-")
	if numeric != "" && numeric[0] >= '0' && numeric[0] <= '9' {
		if iv, err := strconv.ParseInt(v, 10, 64); err == nil {
			return intAttrVal(iv)
		}
		if fv, err := strconv.ParseFloat(v, 64); err == nil && strings.ContainsAny(v, ".eE") {
			return doubleAttrVal(fv)
		}
	}
	// 5. Fallback: string
	return strAttrVal(v)
}

// ── payload preview ────────────────────────────────────────────────────────────

// openPayloadPreview builds a human-readable config summary for the currently
// selected service and navigates to screenPayload. Available from both the list
// and the service editor tab selector (ctrl+q).
// The caller's screen and tabActive state are saved so that closing the preview
// returns exactly where the user came from.
func (m *tui) openPayloadPreview() (tea.Model, tea.Cmd) {
	var svc Service
	cfg := m.cfg
	if m.screen == screenServiceEdit {
		svc = m.buildServiceFromFields()
		// Include the unsaved service in a scratch config so buildOTLPJSON
		// can find it by name via indexServices().
		svcs := append([]Service(nil), cfg.Services...)
		if m.editIdx == -1 {
			svcs = append(svcs, svc)
		} else if m.editIdx < len(svcs) {
			svcs[m.editIdx] = svc
		}
		cfg.Services = svcs
	} else if len(m.cfg.Services) > 0 && m.cursor < len(m.cfg.Services) {
		svc = m.cfg.Services[m.cursor]
	} else {
		return m, nil
	}

	m.payloadSummary = m.buildPayloadPreview(svc)

	traces, metrics, logs, err := buildOTLPJSON(cfg, svc)
	if err != nil {
		m.payloadJSON = sError.Render("  error: " + err.Error())
	} else {
		m.payloadJSON = m.formatOTLPJSON(traces, metrics, logs)
	}

	m.payloadMode = 0
	m.payloadScroll = 0
	m.payloadPrevScreen = m.screen
	m.payloadPrevTabActive = m.tabActive
	m.screen = screenPayload
	return m, nil
}

// updatePayload handles keys while screenPayload is active.
// Arrow keys scroll; tab switches view mode; any other key closes.
func (m *tui) updatePayload(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	text := m.activePayload()
	all := strings.Split(text, "\n")
	pageSize := m.height - 4
	if pageSize < 5 {
		pageSize = 5
	}
	maxScroll := len(all) - pageSize
	if maxScroll < 0 {
		maxScroll = 0
	}
	switch k.String() {
	case "up", "k":
		if m.payloadScroll > 0 {
			m.payloadScroll--
		}
	case "down", "j":
		if m.payloadScroll < maxScroll {
			m.payloadScroll++
		}
	case "pgup", "b":
		m.payloadScroll -= pageSize
		if m.payloadScroll < 0 {
			m.payloadScroll = 0
		}
	case "pgdown", "f":
		m.payloadScroll += pageSize
		if m.payloadScroll > maxScroll {
			m.payloadScroll = maxScroll
		}
	case "tab":
		m.payloadMode = 1 - m.payloadMode
		m.payloadScroll = 0
	case "1":
		m.payloadMode = 0
		m.payloadScroll = 0
	case "2":
		m.payloadMode = 1
		m.payloadScroll = 0
	default:
		m.screen = m.payloadPrevScreen
		m.tabActive = m.payloadPrevTabActive
	}
	return m, nil
}

// activePayload returns the text for the currently selected payload mode.
func (m *tui) activePayload() string {
	if m.payloadMode == 1 {
		return m.payloadJSON
	}
	return m.payloadSummary
}

// payloadTabBar renders the two-tab bar at the top of the payload preview.
// Tab 1 = Config summary, Tab 2 = OTLP JSON. Active tab gets the blue pill.
func (m *tui) payloadTabBar() string {
	tabs := []string{"Config summary", "OTLP JSON"}
	parts := make([]string, len(tabs))
	for i, name := range tabs {
		label := fmt.Sprintf("%d %s", i+1, name)
		if i == m.payloadMode {
			parts[i] = sTabActive.Render(label)
		} else {
			parts[i] = sTabInactive.Render(label)
		}
	}
	sep := sMuted.Render(" │ ")
	return "  " + strings.Join(parts, sep) + "\n  " + m.sepLine()
}

// payloadView renders the payload preview, paginated to the terminal height.
// The tab bar (2 lines) sits above the scrollable area; the footer sits below.
func (m *tui) payloadView() string {
	tabBar := m.payloadTabBar()

	text := m.activePayload()
	all := strings.Split(text, "\n")
	// 2 tab-bar lines + 1 blank + 1 blank before footer + 1 footer = 5 overhead
	pageSize := m.height - 5
	if pageSize < 5 {
		pageSize = 5
	}
	start := m.payloadScroll
	if start < 0 {
		start = 0
	}
	end := start + pageSize
	if end > len(all) {
		end = len(all)
	}
	visible := strings.Join(all[start:end], "\n")

	footer := sHelp.Render(fmt.Sprintf(
		"  tab/1/2 switch  ·  ↑↓/jk scroll  ·  pgup/pgdn page  ·  %d/%d  ·  any other key closes",
		start+1, len(all),
	))
	return tabBar + "\n" + visible + "\n\n" + footer
}

// formatOTLPJSON combines the three protojson sections into one scrollable string.
// No header or footer — payloadView() renders those around the scrolled content.
func (m *tui) formatOTLPJSON(traces, metrics, logs string) string {
	var b strings.Builder
	section := func(label, body string) {
		if body == "" {
			return
		}
		b.WriteString(sBold.Render("  "+label) + "\n\n")
		for _, line := range strings.Split(body, "\n") {
			b.WriteString("  " + line + "\n")
		}
		b.WriteString("\n")
	}
	section("Traces", traces)
	section("Metrics", metrics)
	section("Logs", logs)
	return b.String()
}

// buildPayloadPreview returns a formatted multi-line string showing the
// effective configuration that will be emitted for svc: resource attrs
// (merged global → infra → service), span attrs, metric config, and signals.
func (m *tui) buildPayloadPreview(svc Service) string {
	cfg := m.cfg
	svc = normalizeService(svc)

	var b strings.Builder
	addRow := func(label, value string) {
		b.WriteString(fmt.Sprintf("  %-22s %s\n", label, value))
	}

	// ── service settings ──────────────────────────────────────────────────
	b.WriteString(sBold.Render("  Service: ") + colorForService(svc.Name).Render(svc.Name) + "\n\n")

	signals := "all (spans + metrics + logs)"
	if len(svc.Signals) > 0 {
		sl := append([]string(nil), svc.Signals...)
		sort.Strings(sl)
		signals = strings.Join(sl, " + ")
	}
	addRow("Signals", signals)
	addRow("Interval", fmt.Sprintf("%ds", svc.Interval))
	addRow("Failure rate", fmt.Sprintf("%d%%", svc.FailureRate))

	if svc.Template != "" {
		addRow("Span template", svc.Template)
	}
	addRow("Span kind", svc.SpanKind)
	if svc.ChildSpans > 0 {
		addRow("Local child spans", strconv.Itoa(svc.ChildSpans))
	}
	if svc.InfraTemplate != "" {
		addRow("Infra template", svc.InfraTemplate)
	}
	if svc.Mesh {
		addRow("Istio mesh", "on")
	}
	if len(svc.DownstreamCalls) > 0 {
		addRow("Downstream calls", strings.Join(svc.DownstreamCalls, ", "))
	}
	if svc.hasSignal(signalMetrics) {
		mc := effectiveMetricConfig(svc)
		addRow("Metric", fmt.Sprintf("%s %s (%s)", mc.Name, mc.Unit, mc.Type))
		// Dynatrace entity extraction routes on the metric key, so spell these
		// out rather than leaving the reader to infer them from the template.
		for _, im := range infraMetrics(svc, time.Now()) {
			addRow("", fmt.Sprintf("%s %s%s", im.Name, im.Unit, sMuted.Render("  (from "+svc.InfraTemplate+")")))
		}
	}
	if svc.hasSignal(signalLogs) {
		addRow("Log severity", strings.ToUpper(effectiveLogSeverity(svc)))
	}

	b.WriteString("\n  " + m.sepLine() + "\n\n")

	// ── resource attributes ───────────────────────────────────────────────
	b.WriteString(sBold.Render("  Resource attributes") + sMuted.Render(" (global → infra → service)") + "\n\n")
	if len(cfg.Attributes) > 0 {
		b.WriteString(sMuted.Render("  global:") + "\n")
		for _, k := range sortedKeys(cfg.Attributes) {
			b.WriteString(fmt.Sprintf("    %s = %s\n", k, attrValueText(cfg.Attributes[k], false)))
		}
	}
	infra := infraDefaults(svc)
	if len(infra) > 0 {
		b.WriteString(sMuted.Render(fmt.Sprintf("  %s (infra template):", svc.InfraTemplate)) + "\n")
		for _, k := range sortedKeys(infra) {
			b.WriteString(fmt.Sprintf("    %s = %s\n", k, attrValueText(infra[k], false)))
		}
	}
	b.WriteString(sMuted.Render("  service:") + "\n")
	b.WriteString(fmt.Sprintf("    service.name = %s\n", svc.Name))
	for _, k := range sortedKeys(svc.Attributes) {
		b.WriteString(fmt.Sprintf("    %s = %s\n", k, attrValueText(svc.Attributes[k], false)))
	}

	b.WriteString("\n")

	// ── span attributes ───────────────────────────────────────────────────
	if svc.hasSignal(signalSpans) {
		b.WriteString(sBold.Render("  Span attributes") + sMuted.Render(" (template → overrides)") + "\n\n")
		tmpl := templateDefaults(svc.Template)
		if len(tmpl) > 0 {
			b.WriteString(sMuted.Render(fmt.Sprintf("  %s (template):", svc.Template)) + "\n")
			for _, k := range sortedKeys(tmpl) {
				b.WriteString(fmt.Sprintf("    %s = %s\n", k, attrValueText(tmpl[k], false)))
			}
		}
		if len(svc.SpanAttrs) > 0 {
			b.WriteString(sMuted.Render("  overrides:") + "\n")
			for _, k := range sortedKeys(svc.SpanAttrs) {
				b.WriteString(fmt.Sprintf("    %s = %s\n", k, attrValueText(svc.SpanAttrs[k], false)))
			}
		}
		if len(tmpl) == 0 && len(svc.SpanAttrs) == 0 {
			b.WriteString(sMuted.Render("  none") + "\n")
		}
		b.WriteString("\n")
	}

	return b.String()
}

// sortedKeys returns the map's keys in ascending order.
func sortedKeys(m map[string]AttrValue) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ── misc ──────────────────────────────────────────────────────────────────────

func truncate(s string, n int) string {
	if n <= 1 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
