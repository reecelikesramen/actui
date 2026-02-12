package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"
	"gopkg.in/yaml.v3"
)

// -- Constants & Enums --

const (
	viewWorkflows = iota
	viewJobs
	viewEvents
	viewConfig // New view
	viewRunning
)

const (
	configEvent = iota
	configPlatform
	configArtifact
	configInputs
	configSecrets
	configEnv
	configRun
	configSave
	configCancel
)

// -- Styles --

var (
	appStyle = lipgloss.NewStyle().Padding(0, 2)

	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFDF5")).
			Background(lipgloss.Color("#25A065")).
			Padding(0, 1)

	headerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFDF5")).
			Background(lipgloss.Color("#25A065")).
			Bold(true).
			Padding(0, 1)

	// Tab Styles
	tabBorder = lipgloss.Border{
		Top:         "─",
		Bottom:      "─",
		Left:        "│",
		Right:       "│",
		TopLeft:     "╭",
		TopRight:    "╮",
		BottomLeft:  "┴",
		BottomRight: "┴",
	}

	tabStyle = lipgloss.NewStyle().
			Border(tabBorder, true).
			BorderForeground(lipgloss.Color("63")).
			Padding(0, 1)

	activeTabStyle = tabStyle.
			BorderForeground(lipgloss.Color("62")).
			Foreground(lipgloss.Color("230")).
			Background(lipgloss.Color("62")) // Highlight active tab

	inactiveTabStyle = tabStyle.
				Foreground(lipgloss.Color("240"))

	// Status Styles
	successColor = lipgloss.Color("42")  // Green
	failureColor = lipgloss.Color("196") // Red
	runningColor = lipgloss.Color("230") // Default/White

	tabSuccessStyle = tabStyle.Foreground(successColor)
	tabFailureStyle = tabStyle.Foreground(failureColor)

	activeTabSuccessStyle = activeTabStyle.Foreground(successColor)
	activeTabFailureStyle = activeTabStyle.Foreground(failureColor)
)

// -- Data Models --

type WorkflowFile struct {
	Name string
	File string
}

func (w WorkflowFile) FilterValue() string { return w.Name }
func (w WorkflowFile) Title() string       { return w.Name }
func (w WorkflowFile) Description() string { return w.File }

type Job struct {
	Stage        string
	JobID        string
	JobName      string
	WorkflowName string
	WorkflowFile string
	Events       string
}

func (j Job) FilterValue() string { return j.JobName + " " + j.WorkflowName }
func (j Job) Title() string       { return j.JobName }
func (j Job) Description() string { return fmt.Sprintf("%s (%s)", j.WorkflowName, j.WorkflowFile) }

type EventItem struct {
	Name string
}

func (e EventItem) FilterValue() string { return e.Name }
func (e EventItem) Title() string       { return e.Name }
func (e EventItem) Description() string { return "Simulate " + e.Name + " event" }

// -- Key Maps --

type listKeyMap struct {
	toggleSelect key.Binding // Space
	run          key.Binding // r or enter
	switchMode   key.Binding // tab
}

func newListKeyMap() *listKeyMap {
	return &listKeyMap{
		toggleSelect: key.NewBinding(
			key.WithKeys(" "),
			key.WithHelp("space", "select"),
		),
		run: key.NewBinding(
			key.WithKeys("r", "enter"),
			key.WithHelp("enter", "run"),
		),
		switchMode: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "switch mode"),
		),
	}
}

// -- List Delegate --

type itemDelegate struct {
	keys *listKeyMap
}

func newItemDelegate(keys *listKeyMap) itemDelegate {
	return itemDelegate{keys: keys}
}

func (d itemDelegate) Height() int                               { return 1 }
func (d itemDelegate) Spacing() int                              { return 0 }
func (d itemDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd { return nil }
func (d itemDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	str := ""
	desc := ""

	if i, ok := listItem.(WorkflowFile); ok {
		str = i.Title()
		desc = i.Description()
	} else if i, ok := listItem.(Job); ok {
		str = i.Title()
		desc = i.Description()
	} else if i, ok := listItem.(EventItem); ok {
		str = i.Title()
		desc = i.Description()
	}

	fn := lipgloss.NewStyle().PaddingLeft(4).Render
	if index == m.Index() {
		fn = func(s ...string) string {
			return lipgloss.NewStyle().PaddingLeft(2).Foreground(lipgloss.Color("170")).Render("> " + strings.Join(s, " "))
		}
	}

	fmt.Fprintf(w, "%s %s", fn(str), lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(desc))
}

// -- Main Model --

type model struct {
	// Lists
	workflowFileList list.Model
	jobList          list.Model
	eventList        list.Model

	// State
	viewState int // viewWorkflows, viewJobs, viewEvents, viewRunning
	keys      *listKeyMap
	zm        *zone.Manager

	// Data
	jobs []Job
	workflowsDir string

	// Execution & Logs
	viewport viewport.Model
	cmd      *exec.Cmd
	logChan  chan string
	runTitle string

	// Tabs (Job Logs)
	tabs       []string            // Tab titles (Job IDs)
	logBuffers map[string]string   // Logs per job (Raw text for Overview / fallback)
	jobData    map[string]*JobData // Structured data for jobs
	activeTab  int
	tabOffset  int // For horizontal scrolling of tabs

	// Parsing
	jobRegex *regexp.Regexp

	// UI
	width, height int
	err           error
	loading       bool
	statusMsg     string
	runConfig     RunConfig // Current run configuration
	textInput     textinput.Model
	wrapLogs      bool
	expectedSteps map[string][]string // Pre-parsed from YAML
}

type ConfigItem struct {
	Key   string
	Value string
}

type RunConfig struct {
	Event          string
	WorkflowFile   string
	JobID          string
	Platform       string
	ArtifactServer bool
	Inputs         []ConfigItem
	Secrets        []ConfigItem
	Env            []ConfigItem

	// UI State for Config View
	Cursor    int
	SubCursor int
	Editing   bool
}

type StepStatus int

const (
	StatusPending StepStatus = iota
	StatusRunning
	StatusSuccess
	StatusFailure
	StatusSkipped
)

type JobStatus int

const (
	JobPending JobStatus = iota
	JobRunning
	JobSuccess
	JobFailure
	JobCancelled
)

type StepLog struct {
	Name   string
	Logs   []string
	Status StepStatus
	IsOpen bool
}

type JobData struct {
	Steps []*StepLog
	Status JobStatus
	ExpectedSteps []string // Names of all steps parsed from YAML
}

// YAML structs for parsing workflow inputs

type WorkflowYaml struct {
	On struct {
		WorkflowDispatch struct {
			Inputs map[string]struct {
				Description string `yaml:"description"`
				Default     string `yaml:"default"`
				Required    bool   `yaml:"required"`
			} `yaml:"inputs"`
		} `yaml:"workflow_dispatch"`
	} `yaml:"on"`
	Env  map[string]string `yaml:"env"`
	Jobs map[string]struct {
		Name  string `yaml:"name"`
		Steps []struct {
			Name string `yaml:"name"`
			ID   string `yaml:"id"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// Messages

type jobsMsg []Job
type errMsg error
type runMsg struct {
	sub chan string
	cmd *exec.Cmd
}
type logMsg struct {
	content string
	sub     chan string
}
type finishedMsg struct {
	sub chan string
}

// -- Init --

func initialModel(workflowsDir string) model {
	keys := newListKeyMap()
	delegate := newItemDelegate(keys)

	if workflowsDir == "" {
		workflowsDir = ".github/workflows/"
	}
	// Ensure it has trailing slash for consistency if it's a directory
	// but act -W can take a file too. If it's a directory, we usually want it.
	// However, WorkflowFile logic uses it as a prefix.
	
	// Workflow File List
	wl := list.New([]list.Item{}, delegate, 0, 0)
	wl.SetShowTitle(false)
	wl.SetShowStatusBar(false)
	wl.AdditionalShortHelpKeys = func() []key.Binding { return []key.Binding{keys.run, keys.switchMode} }

	// Job List
	jl := list.New([]list.Item{}, delegate, 0, 0)
	jl.SetShowTitle(false)
	jl.SetShowStatusBar(false)
	jl.AdditionalShortHelpKeys = func() []key.Binding { return []key.Binding{keys.run, keys.switchMode} }

	// Event List
	el := list.New([]list.Item{}, delegate, 0, 0)
	el.SetShowTitle(false)
	el.SetShowStatusBar(false)
	el.AdditionalShortHelpKeys = func() []key.Binding { return []key.Binding{keys.run, keys.switchMode} }

	commonEvents := []list.Item{
		EventItem{Name: "push"},
		EventItem{Name: "pull_request"},
		EventItem{Name: "workflow_dispatch"},
		EventItem{Name: "schedule"},
	}
	el.SetItems(commonEvents)

	vp := viewport.New(0, 0)
	vp.Style = lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		PaddingRight(0)

	re := regexp.MustCompile(`^\[(.*?)\]`)
	zm := zone.New()

	ti := textinput.New()
	ti.Placeholder = "Value"
	ti.Focus()

	return model{
		workflowFileList: wl,
		jobList:          jl,
		eventList:        el,
		keys:             keys,
		viewState:        viewWorkflows,
		loading:          true,
		viewport:         vp,
		jobRegex:         re,
		logBuffers:       make(map[string]string),
		jobData:          make(map[string]*JobData),
		tabs:             []string{},
		zm:               zm,
		textInput:        ti,
		wrapLogs:         true,
		workflowsDir:     workflowsDir,
		runConfig: RunConfig{
			Platform: "linux/amd64",
		},
	}
}

func (m *model) prepareRunConfig(file string, jobID string, event string) {
	m.runConfig = RunConfig{
		WorkflowFile:   file,
		JobID:          jobID,
		Event:          event,
		Platform:       "linux/amd64",
		ArtifactServer: false,
		Cursor:         0,
	}

	if file != "" {
		data, err := os.ReadFile(file)
		if err == nil {
			var wf WorkflowYaml
			content := string(data)
			if err := yaml.Unmarshal(data, &wf); err == nil {
				// Inputs
				for k, v := range wf.On.WorkflowDispatch.Inputs {
					m.runConfig.Inputs = append(m.runConfig.Inputs, ConfigItem{Key: k, Value: v.Default})
				}
				if len(m.runConfig.Inputs) > 0 || strings.Contains(content, "workflow_dispatch:") {
					if event == "" {
						m.runConfig.Event = "workflow_dispatch"
					}
				}

				// Env
				for k, v := range wf.Env {
					m.runConfig.Env = append(m.runConfig.Env, ConfigItem{Key: k, Value: v})
				}
			}

			// Extract Secrets via regex
			secretRegex := regexp.MustCompile(`\$\{\{\s*secrets\.([a-zA-Z0-9_]+)\s*\}\}`)
			matches := secretRegex.FindAllStringSubmatch(content, -1)
			seenSecrets := make(map[string]bool)
			for _, match := range matches {
				if len(match) > 1 {
					secretName := match[1]
					if secretName != "GITHUB_TOKEN" && !seenSecrets[secretName] {
						m.runConfig.Secrets = append(m.runConfig.Secrets, ConfigItem{Key: secretName, Value: ""})
						seenSecrets[secretName] = true
					}
				}
			}
		}
	}

	if m.runConfig.Event == "" {
		m.runConfig.Event = "push"
	}
}

// -- Commands --

func getJobs(workflowsDir string) tea.Cmd {
	return func() tea.Msg {
		args := []string{"-l"}
		if workflowsDir != "" && workflowsDir != ".github/workflows/" {
			args = append(args, "-W", workflowsDir)
		}
		cmd := exec.Command("act", args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return errMsg(fmt.Errorf("failed to run act: %w\nOutput: %s", err, output))
		}

		var jobs []Job
		lines := strings.Split(string(output), "\n")
		headerFound := false
		re := regexp.MustCompile(`\s{2,}`)

		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "Stage") && strings.Contains(line, "Job ID") {
				headerFound = true
				continue
			}
			if !headerFound {
				continue
			}

			parts := re.Split(line, -1)
			if len(parts) >= 6 {
				jobs = append(jobs, Job{
					Stage: parts[0], JobID: parts[1], JobName: parts[2],
					WorkflowName: parts[3], WorkflowFile: parts[4], Events: parts[5],
				})
			}
		}
		return jobsMsg(jobs)
	}
}

func runAct(args ...string) tea.Cmd {
	return func() tea.Msg {
		sub := make(chan string)
		cmd := exec.Command("act", args...)

		// Use standard pipes to force non-interactive mode, which usually ensures
		// consistent log prefixing ([Job Name]) on every line.
		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()

		if err := cmd.Start(); err != nil {
			return errMsg(err)
		}

		go func() {
			scanner := bufio.NewScanner(io.MultiReader(stdout, stderr))
			for scanner.Scan() {
				text := scanner.Text()
				if len(text) > 0 {
					sub <- text + "\n"
				}
			}
			close(sub)
			cmd.Wait()
		}()
		return runMsg{sub, cmd}
	}
}

func waitForActivity(sub chan string) tea.Cmd {
	return func() tea.Msg {
		content, ok := <-sub
		if !ok {
			return finishedMsg{sub}
		}
		return logMsg{content, sub}
	}
}

// -- Update --

func (m model) Init() tea.Cmd {
	return getJobs(m.workflowsDir)
}

func (m model) getConfigItemsCount() int {
	count := 3 // Event, Platform, Artifact
	count += len(m.runConfig.Inputs)
	count += len(m.runConfig.Secrets)
	count += len(m.runConfig.Env)
	count += 3 // Run, Save, Cancel
	return count
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		h, _ := appStyle.GetFrameSize()

		// Header (1) + HelpLine (1) + Potential StatusMsg (1)
		footerHeight := 2
		if m.statusMsg != "" {
			footerHeight = 3
		}
		listHeight := msg.Height - footerHeight - 1 // -1 for Header
		listWidth := msg.Width - h

		if listHeight < 0 {
			listHeight = 0
		}

		m.workflowFileList.SetSize(listWidth, listHeight)
		m.jobList.SetSize(listWidth, listHeight)
		m.eventList.SetSize(listWidth, listHeight)
		m.viewport.Width = listWidth
		m.viewport.Height = listHeight - 3 - 2 // Tabs (3) + Viewport Borders (2)
		if m.viewState == viewRunning {
			m.updateViewportContent()
		}
	case tea.KeyMsg:
		if m.viewState == viewRunning {
			switch msg.String() {
			case "esc", "q":
				if m.cmd != nil && m.cmd.Process != nil {
					m.cmd.Process.Kill()
				}
				m.viewState = viewWorkflows
				m.statusMsg = "Run finished/cancelled."
				return m, nil
			case "w":
				m.wrapLogs = !m.wrapLogs
				m.updateViewportContent()
				return m, nil
			case "c":
				var content string
				if m.activeTab == 0 {
					content = m.logBuffers["Overview"]
				} else {
					jobID := m.tabs[m.activeTab]
					content = m.logBuffers[jobID]
				}
				if content != "" {
					clipboard.WriteAll(content)
					m.statusMsg = "Logs copied to clipboard!"
				}
				return m, nil
			case "tab":
				if len(m.tabs) > 0 {
					m.activeTab = (m.activeTab + 1) % len(m.tabs)
					m.updateViewportContent()
					m.viewport.GotoBottom()
				}
				return m, nil
			case "shift+tab":
				if len(m.tabs) > 0 {
					m.activeTab--
					if m.activeTab < 0 {
						m.activeTab = len(m.tabs) - 1
					}
					m.updateViewportContent()
					m.viewport.GotoBottom()
				}
				return m, nil
			default:
				m.viewport, cmd = m.viewport.Update(msg)
				return m, cmd
			}
		} else if m.viewState == viewConfig {
			if m.runConfig.Editing {
				switch msg.String() {
				case "enter":
					m.runConfig.Editing = false
					val := m.textInput.Value()

					// Update the item
					idx := m.runConfig.Cursor
					offset := 3 // Event, Platform, Artifact
					if idx >= offset && idx < offset+len(m.runConfig.Inputs) {
						m.runConfig.Inputs[idx-offset].Value = val
					} else {
						offset += len(m.runConfig.Inputs)
						if idx >= offset && idx < offset+len(m.runConfig.Secrets) {
							m.runConfig.Secrets[idx-offset].Value = val
						} else {
							offset += len(m.runConfig.Secrets)
							if idx >= offset && idx < offset+len(m.runConfig.Env) {
								m.runConfig.Env[idx-offset].Value = val
							}
						}
					}
					return m, nil
				case "esc":
					m.runConfig.Editing = false
					return m, nil
				}
				m.textInput, cmd = m.textInput.Update(msg)
				return m, cmd
			}

			switch msg.String() {
			case "esc", "q":
				m.viewState = viewWorkflows
				return m, nil
			case "up", "k":
				if m.runConfig.Cursor > 0 {
					m.runConfig.Cursor--
				}
			case "down", "j":
				if m.runConfig.Cursor < m.getConfigItemsCount()-1 {
					m.runConfig.Cursor++
				}
			case "enter":
				idx := m.runConfig.Cursor
				// 0: Event, 1: Platform, 2: Artifact
				switch idx {
				case 0:
					events := []string{"push", "pull_request", "workflow_dispatch", "schedule", "release"}
					found := -1
					for i, e := range events {
						if e == m.runConfig.Event {
							found = i
							break
						}
					}
					m.runConfig.Event = events[(found+1)%len(events)]
					return m, nil
				case 1:
					if m.runConfig.Platform == "linux/amd64" {
						m.runConfig.Platform = "linux/arm64"
					} else {
						m.runConfig.Platform = "linux/amd64"
					}
					return m, nil
				case 2:
					m.runConfig.ArtifactServer = !m.runConfig.ArtifactServer
					return m, nil
				}

				offset := 3
				// Inputs
				if idx >= offset && idx < offset+len(m.runConfig.Inputs) {
					m.runConfig.Editing = true
					m.textInput.SetValue(m.runConfig.Inputs[idx-offset].Value)
					m.textInput.Focus()
					return m, nil
				}
				offset += len(m.runConfig.Inputs)

				// Secrets
				if idx >= offset && idx < offset+len(m.runConfig.Secrets) {
					m.runConfig.Editing = true
					m.textInput.SetValue(m.runConfig.Secrets[idx-offset].Value)
					m.textInput.Focus()
					return m, nil
				}
				offset += len(m.runConfig.Secrets)

				// Env
				if idx >= offset && idx < offset+len(m.runConfig.Env) {
					m.runConfig.Editing = true
					m.textInput.SetValue(m.runConfig.Env[idx-offset].Value)
					m.textInput.Focus()
					return m, nil
				}
				offset += len(m.runConfig.Env)

				// Actions
				if idx == offset {
					// RUN
					m.viewState = viewRunning
					m.logBuffers = make(map[string]string)
					m.jobData = make(map[string]*JobData)
					m.tabs = []string{"Overview"}
					m.activeTab = 0

					// Pre-parse steps from YAML if possible
					m.expectedSteps = make(map[string][]string)
					if m.runConfig.WorkflowFile != "" {
						if data, err := os.ReadFile(m.runConfig.WorkflowFile); err == nil {
							var wf WorkflowYaml
							if err := yaml.Unmarshal(data, &wf); err == nil {
								for jobID, job := range wf.Jobs {
									var steps []string
									steps = append(steps, "Set up job")
									for _, s := range job.Steps {
										name := s.Name
										if name == "" {
											name = s.ID
										}
										if name == "" {
											// Fallback to a truncated version of the run command
											name = s.Run
											if len(name) > 30 {
												name = name[:27] + "..."
											}
										}
										if name != "" {
											steps = append(steps, name)
										}
									}
									steps = append(steps, "Complete job")
									m.expectedSteps[jobID] = steps
									if job.Name != "" {
										m.expectedSteps[job.Name] = steps
									}
								}
							}
						}
					}

					// Build Args
					args := []string{}
					if m.workflowsDir != "" && m.workflowsDir != ".github/workflows/" {
						args = append(args, "-W", m.workflowsDir)
					}
					if m.runConfig.JobID != "" {
						args = append(args, "-j", m.runConfig.JobID)
					} else if m.runConfig.WorkflowFile != "" {
						args = append(args, "-W", m.runConfig.WorkflowFile)
					}

					if m.runConfig.JobID == "" && m.runConfig.Event != "" {
						args = append([]string{m.runConfig.Event}, args...)
					}

					if m.runConfig.Platform != "" {
						args = append(args, "--container-architecture", m.runConfig.Platform)
					}

					if m.runConfig.ArtifactServer {
						args = append(args, "--artifact-server-path", "/tmp/act-artifacts")
					}

					for _, item := range m.runConfig.Inputs {
						if item.Value != "" {
							args = append(args, "--input", fmt.Sprintf("%s=%s", item.Key, item.Value))
						}
					}
					for _, item := range m.runConfig.Secrets {
						if item.Value != "" {
							args = append(args, "--secret", fmt.Sprintf("%s=%s", item.Key, item.Value))
						}
					}
					for _, item := range m.runConfig.Env {
						if item.Value != "" {
							args = append(args, "--env", fmt.Sprintf("%s=%s", item.Key, item.Value))
						}
					}

					m.logBuffers["Overview"] = "Starting run with args: " + strings.Join(args, " ") + "\n"
					m.viewport.SetContent(m.logBuffers["Overview"])
					return m, runAct(args...)
				}
				if idx == offset+1 {
					// SAVE PROFILE - TODO: Implement persistence
					m.statusMsg = "Profile saved (simulated)."
					return m, nil
				}
				if idx == offset+2 {
					// CANCEL
					m.viewState = viewWorkflows
					return m, nil
				}
			}
			return m, nil
		}

		if m.workflowFileList.FilterState() != list.Filtering &&
			m.jobList.FilterState() != list.Filtering &&
			m.eventList.FilterState() != list.Filtering {

			switch {
			case key.Matches(msg, m.keys.switchMode):
				m.viewState = (m.viewState + 1) % 3 // Cycle 0->1->2->0
				return m, nil

			case key.Matches(msg, m.keys.run):
				var file, jobID, event string

				switch m.viewState {
				case viewWorkflows:
					if i, ok := m.workflowFileList.SelectedItem().(WorkflowFile); ok {
						dir := m.workflowsDir
						if !strings.HasSuffix(dir, "/") {
							dir += "/"
						}
						file = dir + i.File
						m.runTitle = "Workflow: " + i.Name
					}
				case viewJobs:
					if i, ok := m.jobList.SelectedItem().(Job); ok {
						jobID = i.JobID
						m.runTitle = "Job: " + i.JobName
						// Ensure we have the full path for parsing
						file = i.WorkflowFile
						if !strings.Contains(file, "/") {
							dir := m.workflowsDir
							if !strings.HasSuffix(dir, "/") {
								dir += "/"
							}
							file = dir + file
						}
					}
				case viewEvents:
					if i, ok := m.eventList.SelectedItem().(EventItem); ok {
						event = i.Name
						m.runTitle = "Event: " + i.Name
					}
				}

				if file != "" || jobID != "" || event != "" {
					m.prepareRunConfig(file, jobID, event)
					m.viewState = viewConfig
					return m, nil
				}
			}
		}

	case jobsMsg:
		m.loading = false
		m.jobs = msg

		// Populate Job List
		jobItems := make([]list.Item, len(msg))
		for i, j := range msg {
			jobItems[i] = j
		}
		m.jobList.SetItems(jobItems)

		// Populate Workflow File List (Deduplicate)
		seenFiles := make(map[string]bool)
		var fileItems []list.Item
		for _, j := range msg {
			if !seenFiles[j.WorkflowFile] {
				seenFiles[j.WorkflowFile] = true
				fileItems = append(fileItems, WorkflowFile{
					Name: j.WorkflowName,
					File: j.WorkflowFile,
				})
			}
		}
		m.workflowFileList.SetItems(fileItems)

	case runMsg:
		m.logChan = msg.sub
		m.cmd = msg.cmd
		return m, waitForActivity(m.logChan)

	case logMsg:
		if msg.sub == m.logChan {
			m.logBuffers["Overview"] += msg.content

			lines := strings.Split(msg.content, "\n")
			for _, line := range lines {
				cleanLine := stripAnsi(line)
				matches := m.jobRegex.FindStringSubmatch(cleanLine)
				if len(matches) > 1 {
					jobID := matches[1]

					// Initialize JobData if needed
					if _, exists := m.jobData[jobID]; !exists {
						// Fallback: if exact jobID isn't in expectedSteps, try fuzzy match
						// (sometimes act adds prefixes or suffixes)
						steps := m.expectedSteps[jobID]
						if steps == nil {
							for k, v := range m.expectedSteps {
								if strings.Contains(jobID, k) || strings.Contains(k, jobID) {
									steps = v
									break
								}
							}
						}

						m.jobData[jobID] = &JobData{
							Steps:         []*StepLog{},
							Status:        JobRunning,
							ExpectedSteps: steps,
						}
					}
					jd := m.jobData[jobID]
					if jd.Status == JobPending {
						jd.Status = JobRunning
					}

					// Ensure tab exists
					found := false
					for _, t := range m.tabs {
						if t == jobID {
							found = true
							break
						}
					}
					if !found {
						m.tabs = append(m.tabs, jobID)
						m.logBuffers[jobID] = ""
					}

					content := line[len(matches[0]):]
					text := strings.TrimSpace(content)
					cleanText := stripAnsi(text)

					// Ignore lines that are clearly log content (usually start with |)
					isLogLine := strings.HasPrefix(text, "|")

					// Check for New Step: Usually starts with a star or "Run "
					isNewStep := false
					if !isLogLine && strings.Contains(cleanText, "Run ") {
						// Act markers are usually "⭐ Run" or at the start of the line after the [Job] prefix.
						if strings.HasPrefix(cleanText, "Run ") || strings.Contains(cleanText, " \u2b50 Run ") || strings.Contains(cleanText, " Run ") {
							isNewStep = true
						}
					}

					if isNewStep {
						stepName := cleanText
						if idx := strings.Index(cleanText, "Run "); idx != -1 {
							stepName = cleanText[idx+4:]
						}
						
						// Remove common prefixes added by act
						for _, prefix := range []string{"Main ", "Pre ", "Post "} {
							if strings.HasPrefix(stepName, prefix) {
								stepName = strings.TrimPrefix(stepName, prefix)
								break
							}
						}

						// Close previous step if it was running (Assumption: success unless we saw failure)
						if len(jd.Steps) > 0 {
							last := jd.Steps[len(jd.Steps)-1]
							if last.Status == StatusRunning {
								last.Status = StatusSuccess
								last.IsOpen = false
							}
						}

						step := &StepLog{
							Name:   stepName,
							Status: StatusRunning,
							IsOpen: true,
							Logs:   []string{},
						}
						jd.Steps = append(jd.Steps, step)

					} else if strings.Contains(cleanText, "Skipping step:") {
						stepName := cleanText
						if idx := strings.Index(cleanText, "Skipping step: "); idx != -1 {
							stepName = cleanText[idx+15:]
						}
						step := &StepLog{
							Name:   stepName,
							Status: StatusSkipped,
							IsOpen: false,
							Logs:   []string{"Step skipped by act (likely due to 'if' condition)"},
						}
						jd.Steps = append(jd.Steps, step)
					} else if strings.Contains(cleanText, "Success -") || strings.Contains(cleanText, "\u2705") {
						if len(jd.Steps) > 0 {
							last := jd.Steps[len(jd.Steps)-1]
							last.Status = StatusSuccess
							last.IsOpen = false
						}
					} else if strings.Contains(cleanText, "Failure -") || strings.Contains(cleanText, "\u274c") || strings.Contains(cleanText, "\u274e") || strings.Contains(cleanText, "\u2716") {
						if len(jd.Steps) > 0 {
							last := jd.Steps[len(jd.Steps)-1]
							last.Status = StatusFailure
							last.IsOpen = true
						}
					} else if strings.Contains(cleanText, "Job succeeded") {
						jd.Status = JobSuccess
					} else if strings.Contains(cleanText, "Job failed") {
						jd.Status = JobFailure
						// If the job failed, the current step must have failed
						if len(jd.Steps) > 0 {
							last := jd.Steps[len(jd.Steps)-1]
							if last.Status == StatusRunning {
								last.Status = StatusFailure
								last.IsOpen = true
							}
						}
					} else {
						// Raw log buffer for copying
						m.logBuffers[jobID] += content + "\n"

						if len(jd.Steps) > 0 {
							last := jd.Steps[len(jd.Steps)-1]
							logLine := strings.TrimLeft(content, " ")
							if strings.HasPrefix(logLine, "|") {
								logLine = logLine[1:]
							}
							if len(logLine) > 0 && logLine[0] == ' ' {
								logLine = logLine[1:]
							}
							last.Logs = append(last.Logs, logLine)
						}
					}
				}
			}

			m.updateViewportContent()
			if m.activeTab == 0 || (m.activeTab > 0 && m.isTailFollowing()) {
				m.viewport.GotoBottom()
			}
			return m, waitForActivity(m.logChan)
		}

	case finishedMsg:
		if msg.sub == m.logChan {
			m.logBuffers["Overview"] += "\nDone."
			
			// Mark any remaining Pending jobs as Cancelled
			for _, jd := range m.jobData {
				if jd.Status == JobPending {
					jd.Status = JobCancelled
				}
			}

			if m.activeTab < len(m.tabs) {
				m.updateViewportContent()
				m.viewport.GotoBottom()
			}
			m.cmd = nil
		}

	case tea.MouseMsg:
		if m.viewState == viewRunning {
			if msg.Type == tea.MouseLeft {
				// Check Tabs
				clickedTab := -1
				for i, t := range m.tabs {
					if m.zm.Get(t).InBounds(msg) {
						clickedTab = i
						break
					}
				}
				if clickedTab != -1 {
					m.activeTab = clickedTab
					m.updateViewportContent()
					m.viewport.GotoBottom()
					return m, nil
				}

				// Check Step Headers (Only if active tab is Job)
				if m.activeTab > 0 {
					currentJob := m.tabs[m.activeTab]
					if jd, ok := m.jobData[currentJob]; ok {
						for i, step := range jd.Steps {
							zoneID := fmt.Sprintf("%s_step_%d", currentJob, i)
							if m.zm.Get(zoneID).InBounds(msg) {
								step.IsOpen = !step.IsOpen
								m.updateViewportContent()
								// Do not auto scroll on toggle usually
								return m, nil
							}
						}
					}
				}
			}
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}

	case errMsg:
		m.err = msg
		m.loading = false
		return m, nil
	}

	if m.viewState == viewWorkflows {
		m.workflowFileList, cmd = m.workflowFileList.Update(msg)
		cmds = append(cmds, cmd)
	} else if m.viewState == viewJobs {
		m.jobList, cmd = m.jobList.Update(msg)
		cmds = append(cmds, cmd)
	} else if m.viewState == viewEvents {
		m.eventList, cmd = m.eventList.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m *model) updateViewportContent() {
	// The viewport has a border (2 columns)
	wrapWidth := m.viewport.Width - 2
	if wrapWidth < 10 {
		wrapWidth = 10
	}

	if m.activeTab == 0 {
		// Render Overview: Job status summary + Last few logs
		var s strings.Builder
		for _, t := range m.tabs {
			if t == "Overview" {
				continue
			}
			jd, ok := m.jobData[t]
			if !ok || jd.Status == JobPending {
				continue
			}

			statusIcon := "○"
			statusColor := "252"
			if jd.Status == JobRunning {
				statusIcon = "🔄"
				statusColor = "230"
			} else if jd.Status == JobSuccess {
				statusIcon = "✅"
				statusColor = "42"
			} else if jd.Status == JobFailure {
				statusIcon = "❌"
				statusColor = "196"
			} else if jd.Status == JobCancelled {
				statusIcon = "🚫"
				statusColor = "240"
			}

			displayName := t
			if idx := strings.LastIndex(t, "/"); idx != -1 {
				displayName = t[idx+1:]
			}

			header := fmt.Sprintf("%s %s", statusIcon, displayName)
			if jd.Status == JobCancelled {
				header += " (Cancelled)"
			}
			s.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(statusColor)).Bold(true).Render(header) + "\n")

			// Show current/last step
			if len(jd.Steps) > 0 {
				lastStep := jd.Steps[len(jd.Steps)-1]
				stepInfo := fmt.Sprintf("  ↳ %s", lastStep.Name)
				if lastStep.Status == StatusRunning && len(lastStep.Logs) > 0 {
					lastLog := lastStep.Logs[len(lastStep.Logs)-1]
					if len(lastLog) > 60 {
						lastLog = lastLog[:57] + "..."
					}
					stepInfo += lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(" | " + lastLog)
				}
				s.WriteString(stepInfo + "\n")
			}
			s.WriteString("\n")
		}

		s.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("--- Full Logs ---") + "\n")

		logContent := m.logBuffers["Overview"]
		if m.wrapLogs {
			s.WriteString(lipgloss.NewStyle().Width(wrapWidth).Render(logContent))
		} else {
			s.WriteString(logContent)
		}

		m.viewport.SetContent(s.String())
		return
	}

	// Render Job Steps
	jobID := m.tabs[m.activeTab]
	jd, ok := m.jobData[jobID]
	if !ok {
		m.viewport.SetContent("No data for " + jobID)
		return
	}

	var s strings.Builder
	// Track which steps we've already rendered from logs
	renderedSteps := make(map[string]bool)

	for i, step := range jd.Steps {
		renderedSteps[step.Name] = true
		icon := "○"
		if step.Status == StatusRunning {
			icon = "🔄"
		} else if step.Status == StatusSuccess {
			icon = "✅"
		} else if step.Status == StatusFailure {
			icon = "❌"
		} else if step.Status == StatusSkipped {
			icon = "⏩"
		}

		caret := "▶"
		if step.IsOpen {
			caret = "▼"
		}

		header := fmt.Sprintf("%s %s %s", caret, icon, step.Name)
		style := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))

		// Mark Header Zone
		zoneID := fmt.Sprintf("%s_step_%d", jobID, i)
		s.WriteString(m.zm.Mark(zoneID, style.Render(header)) + "\n")

		if step.IsOpen {
			logBlock := strings.Join(step.Logs, "\n")
			logStyle := lipgloss.NewStyle().
				PaddingLeft(2).
				Foreground(lipgloss.Color("245"))

			if m.wrapLogs {
				logStyle = logStyle.Width(wrapWidth)
			}
			s.WriteString(logStyle.Render(logBlock) + "\n")
		}
	}

	// Show Pending/Cancelled Steps
	if len(jd.ExpectedSteps) > 0 && jd.Status != JobSuccess {
		pendingHeader := false
		for _, name := range jd.ExpectedSteps {
			if !renderedSteps[name] {
				if !pendingHeader {
					headerText := "Pending Steps:"
					if jd.Status == JobFailure {
						headerText = "Cancelled Steps (Job Failed):"
					}
					s.WriteString("\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(headerText) + "\n")
					pendingHeader = true
				}
				
				icon := "○"
				style := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
				if jd.Status == JobFailure {
					icon = "🚫"
					style = style.Strikethrough(true)
				}
				
				s.WriteString(style.Render(fmt.Sprintf("  %s %s", icon, name)) + "\n")
			}
		}
	}

	m.viewport.SetContent(s.String())
}

func (m model) isTailFollowing() bool {
	return m.viewport.YOffset >= m.viewport.TotalLineCount()-m.viewport.Height
}

func stripAnsi(str string) string {
	const ansi = "[\u001B\u009B][[\\]()#;?]*(?:(?:(?:[a-zA-Z\\d]*(?:;[a-zA-Z\\d]*)*)?\u0007)|(?:(?:\\d{1,4}(?:;\\d{0,4})*)?[\\dA-PRZcf-ntqry=><~]))"
	var re = regexp.MustCompile(ansi)
	return re.ReplaceAllString(str, "")
}

func (m model) renderConfigView() string {
	s := titleStyle.Render("Run Configuration") + "\n\n"

	renderOption := func(idx int, label string, value string, isEditing bool) string {
		cursor := "  "
		if m.runConfig.Cursor == idx {
			cursor = "> "
		}
		
		text := label
		if value != "" {
			text = fmt.Sprintf("%-20s %s", label+":", value)
		}

		if isEditing && m.runConfig.Cursor == idx {
			return fmt.Sprintf("%s%-20s %s\n", cursor, label+":", m.textInput.View())
		}

		if m.runConfig.Cursor == idx {
			return lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Render(cursor+text) + "\n"
		}
		return cursor + text + "\n"
	}

	currIdx := 0
	
	// Basic Settings
	s += renderOption(currIdx, "Event", m.runConfig.Event, false)
	currIdx++
	s += renderOption(currIdx, "Platform", m.runConfig.Platform, false)
	currIdx++
	s += renderOption(currIdx, "Artifact Server", fmt.Sprintf("%v", m.runConfig.ArtifactServer), false)
	currIdx++

	s += "\n"

	// Inputs
	if len(m.runConfig.Inputs) > 0 {
		s += lipgloss.NewStyle().Bold(true).Render("Inputs:") + "\n"
		for _, item := range m.runConfig.Inputs {
			s += renderOption(currIdx, "  "+item.Key, item.Value, m.runConfig.Editing)
			currIdx++
		}
		s += "\n"
	}

	// Secrets
	if len(m.runConfig.Secrets) > 0 {
		s += lipgloss.NewStyle().Bold(true).Render("Secrets:") + "\n"
		for _, item := range m.runConfig.Secrets {
			val := item.Value
			if val != "" {
				val = "********"
			}
			s += renderOption(currIdx, "  "+item.Key, val, m.runConfig.Editing)
			currIdx++
		}
		s += "\n"
	}

	// Env
	if len(m.runConfig.Env) > 0 {
		s += lipgloss.NewStyle().Bold(true).Render("Env:") + "\n"
		for _, item := range m.runConfig.Env {
			s += renderOption(currIdx, "  "+item.Key, item.Value, m.runConfig.Editing)
			currIdx++
		}
		s += "\n"
	}

	// Actions
	s += renderOption(currIdx, "[ RUN ]", "", false)
	currIdx++
	s += renderOption(currIdx, "[ SAVE PROFILE ]", "", false)
	currIdx++
	s += renderOption(currIdx, "[ CANCEL ]", "", false)

	return appStyle.Render(s)
}

// -- View --

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("Error: %v\nPress q to quit.", m.err)
	}

	// Fallback for width/height if not yet set
	if m.width == 0 {
		return "Initializing..."
	}

	var s string

	if m.viewState == viewRunning {
		// Render Consistent Header for Running View
		header := headerStyle.Width(m.width).Align(lipgloss.Center).Render(m.runTitle)

		// Render Tabs

		// All Tab Status
		allStatus := JobSuccess // Default to success, check if any running or failed
		anyRunning := false
		anyFailed := false

		if len(m.jobData) == 0 {
			allStatus = JobRunning
		} else {
			for _, jd := range m.jobData {
				if jd.Status == JobFailure {
					anyFailed = true
				} else if jd.Status == JobRunning {
					anyRunning = true
				}
			}
			if anyFailed {
				allStatus = JobFailure
			} else if anyRunning {
				allStatus = JobRunning
			}
		}

		// Prepare all tab strings
		tabStrings := make([]string, len(m.tabs))

		// Overview Tab
		allStyle := inactiveTabStyle
		if m.activeTab == 0 {
			allStyle = activeTabStyle
		}
		if allStatus == JobSuccess {
			if m.activeTab == 0 {
				allStyle = activeTabSuccessStyle
			} else {
				allStyle = tabSuccessStyle
			}
		} else if allStatus == JobFailure {
			if m.activeTab == 0 {
				allStyle = activeTabFailureStyle
			} else {
				allStyle = tabFailureStyle
			}
		}
		tabStrings[0] = m.zm.Mark("Overview", allStyle.Render("Overview"))

		// Job Tabs
		for i, t := range m.tabs {
			if i == 0 {
				continue
			}

			var style lipgloss.Style
			if i == m.activeTab {
				style = activeTabStyle
			} else {
				style = inactiveTabStyle
			}

			if jd, ok := m.jobData[t]; ok {
				if jd.Status == JobSuccess {
					if i == m.activeTab {
						style = activeTabSuccessStyle
					} else {
						style = tabSuccessStyle
					}
									} else if jd.Status == JobFailure {
										if i == m.activeTab {
											style = activeTabFailureStyle
										} else {
											style = tabFailureStyle
										}
									} else if jd.Status == JobCancelled {
										style = style.Foreground(lipgloss.Color("240"))
									}
				
			}

			displayName := t
			if idx := strings.LastIndex(t, "/"); idx != -1 {
				displayName = t[idx+1:]
			}
			tabStrings[i] = m.zm.Mark(t, style.Render(displayName))
		}

		// Horizontal Scrolling Logic for Tabs
		availableWidth := m.width - 4 // Padding
		visibleTabs := []string{}
		currentWidth := 0

		// Ensure active tab is within offset
		if m.activeTab < m.tabOffset {
			m.tabOffset = m.activeTab
		}

		// Calculate how many tabs fit starting from tabOffset
		lastVisible := m.tabOffset
		for i := m.tabOffset; i < len(tabStrings); i++ {
			w := lipgloss.Width(tabStrings[i])
			if currentWidth+w > availableWidth-6 { // -6 for indicators
				break
			}
			visibleTabs = append(visibleTabs, tabStrings[i])
			currentWidth += w
			lastVisible = i
		}

		// If active tab is beyond current visible range, shift offset
		if m.activeTab > lastVisible {
			// Find new offset that includes activeTab at the end
			newOffset := m.activeTab
			widthAcc := 0
			for i := m.activeTab; i >= 0; i-- {
				w := lipgloss.Width(tabStrings[i])
				if widthAcc+w > availableWidth-6 {
					newOffset = i + 1
					break
				}
				widthAcc += w
				newOffset = i
			}
			m.tabOffset = newOffset
			
			// Recalculate visibleTabs with new offset
			visibleTabs = []string{}
			currentWidth = 0
			for i := m.tabOffset; i < len(tabStrings); i++ {
				w := lipgloss.Width(tabStrings[i])
				if currentWidth+w > availableWidth-6 {
					break
				}
				visibleTabs = append(visibleTabs, tabStrings[i])
				currentWidth += w
			}
		}

		// Add indicators
		leftInd := "  "
		if m.tabOffset > 0 {
			leftInd = "◀ "
		}
		rightInd := "  "
		if len(tabStrings) > 0 && (m.tabOffset+len(visibleTabs)) < len(tabStrings) {
			rightInd = " ▶"
		}

		row := lipgloss.JoinHorizontal(lipgloss.Top, leftInd)
		row = lipgloss.JoinHorizontal(lipgloss.Top, row, lipgloss.JoinHorizontal(lipgloss.Top, visibleTabs...))
		row = lipgloss.JoinHorizontal(lipgloss.Top, row, rightInd)

		content := lipgloss.JoinVertical(lipgloss.Left, row, m.viewport.View())
		s = lipgloss.JoinVertical(lipgloss.Left, header, appStyle.Render(content))

	} else if m.viewState == viewConfig {
		s = m.renderConfigView()
	} else {
		// Main Menu Views (Workflows, Jobs, Events)
		var headerText string
		var content string

		switch m.viewState {
		case viewWorkflows:
			headerText = "ACTUI - Workflows"
			content = m.workflowFileList.View()
		case viewJobs:
			headerText = "ACTUI - Jobs"
			content = m.jobList.View()
		case viewEvents:
			headerText = "ACTUI - Events"
			content = m.eventList.View()
		default:
			headerText = "ACTUI"
			content = m.workflowFileList.View()
		}

		header := headerStyle.Width(m.width).Align(lipgloss.Center).Render(headerText)
		// Join WITHOUT newline, as JoinVertical adds it
		s = lipgloss.JoinVertical(lipgloss.Left, header, appStyle.Render(content))
	}

	if m.statusMsg != "" {
		s = lipgloss.JoinVertical(lipgloss.Left, s, lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Padding(0, 2).Render(m.statusMsg))
	}

	// Add persistent help/status line at the very bottom
	wrapStatus := "wrap:off"
	if m.wrapLogs {
		wrapStatus = "wrap:on"
	}
	helpLine := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		Padding(0, 2).
		Render(fmt.Sprintf("q: back • w: toggle wrap (%s) • c: copy logs • tab: switch tabs", wrapStatus))
	
	s = lipgloss.JoinVertical(lipgloss.Left, s, helpLine)

	return m.zm.Scan(s)
}

func main() {
	workflowsDir := flag.String("W", ".github/workflows/", "path to workflows directory")
	flag.Parse()

	f, err := tea.LogToFile("debug.log", "debug")
	if err != nil {
		fmt.Println("fatal:", err)
		os.Exit(1)
	}
	defer f.Close()

	p := tea.NewProgram(initialModel(*workflowsDir), tea.WithAltScreen(), tea.WithMouseAllMotion())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Alas, there's been an error: %v", err)
		os.Exit(1)
	}
}
