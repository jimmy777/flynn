package installer

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/flynn/flynn/pkg/random"
)

type httpPrompt struct {
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
	Message  string `json:"message,omitempty"`
	Yes      bool   `json:"yes,omitempty"`
	Input    string `json:"input,omitempty"`
	Resolved bool   `json:"resolved,omitempty"`
	resChan  chan *httpPrompt
	cluster  *Cluster
}

func (prompt *httpPrompt) Resolve(res *httpPrompt) {
	prompt.Resolved = true
	prompt.resChan <- res
}

type httpEvent struct {
	ID          string      `json:"id"`
	Timestamp   time.Time   `json:"timestamp"`
	Type        string      `json:"type"`
	ClusterID   string      `json:"cluster_id",omitempty`
	Description string      `json:"description,omitempty"`
	Prompt      *httpPrompt `json:"prompt,omitempty"`
	Cluster     *Cluster    `json:"cluster,omitempty"`
}

func (event *httpEvent) EventID() string {
	return event.ID
}

type Subscription struct {
	LastEventID string
	EventChan   chan *httpEvent
	DoneChan    chan struct{}
}

func (sub *Subscription) SendEvents(i *Installer) {
	for _, event := range i.GetEventsSince(sub.LastEventID) {
		sub.LastEventID = event.ID
		sub.EventChan <- event
	}
}

func (i *Installer) Subscribe(eventChan chan *httpEvent, lastEventID string) {
	i.subscribeMtx.Lock()
	defer i.subscribeMtx.Unlock()

	subscription := &Subscription{
		LastEventID: lastEventID,
		EventChan:   eventChan,
	}

	go func() {
		subscription.SendEvents(i)
	}()

	i.subscriptions = append(i.subscriptions, subscription)
}

func (i *Installer) GetEventsSince(eventID string) []*httpEvent {
	events := make([]*httpEvent, 0, len(i.events))
	var ts time.Time
	if eventID != "" {
		nano, err := strconv.ParseInt(strings.TrimPrefix(eventID, "event-"), 10, 64)
		if err != nil {
			i.logger.Debug(fmt.Sprintf("Error parsing event id: %s", err.Error()))
		} else {
			ts = time.Unix(0, nano)
		}
	}
	for _, event := range i.events {
		if !event.Timestamp.After(ts) {
			continue
		}
		if event.Type == "install_log" {
			if c, err := i.FindCluster(event.ClusterID); err != nil || (err == nil && c.State == "running") {
				continue
			}
		}
		events = append(events, event)
	}
	return events
}

func (i *Installer) SendEvent(event *httpEvent) {
	event.Timestamp = time.Now()
	event.ID = fmt.Sprintf("event-%d", event.Timestamp.UnixNano())
	i.eventsMtx.Lock()
	i.events = append(i.events, event)
	i.eventsMtx.Unlock()

	for _, sub := range i.subscriptions {
		go sub.SendEvents(i)
	}
}

func (s *Cluster) findPrompt(id string) (*httpPrompt, error) {
	s.promptsMutex.Lock()
	defer s.promptsMutex.Unlock()
	for _, p := range s.Prompts {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, errors.New("Prompt not found")
}

func (s *Cluster) addPrompt(prompt *httpPrompt) {
	s.promptsMutex.Lock()
	defer s.promptsMutex.Unlock()
	s.Prompts = append(s.Prompts, prompt)
}

func (s *Cluster) YesNoPrompt(msg string) bool {
	prompt := &httpPrompt{
		ID:      random.Hex(16),
		Type:    "yes_no",
		Message: msg,
		resChan: make(chan *httpPrompt),
		cluster: s,
	}
	s.addPrompt(prompt)

	s.sendEvent(&httpEvent{
		Type:      "prompt",
		ClusterID: s.ID,
		Prompt:    prompt,
	})

	res := <-prompt.resChan

	s.sendEvent(&httpEvent{
		Type:      "prompt",
		ClusterID: s.ID,
		Prompt:    prompt,
	})

	return res.Yes
}

func (s *Cluster) PromptInput(msg string) string {
	prompt := &httpPrompt{
		ID:      random.Hex(16),
		Type:    "input",
		Message: msg,
		resChan: make(chan *httpPrompt),
		cluster: s,
	}
	s.addPrompt(prompt)

	s.sendEvent(&httpEvent{
		Type:      "prompt",
		ClusterID: s.ID,
		Prompt:    prompt,
	})

	res := <-prompt.resChan

	s.sendEvent(&httpEvent{
		Type:      "prompt",
		ClusterID: s.ID,
		Prompt:    prompt,
	})

	return res.Input
}

func (s *Cluster) sendEvent(event *httpEvent) {
	s.installer.SendEvent(event)
}

func (s *Cluster) SendInstallLogEvent(description string) {
	s.sendEvent(&httpEvent{
		Type:        "install_log",
		ClusterID:   s.ID,
		Description: description,
	})
}

func (s *Cluster) SendError(err error) {
	s.sendEvent(&httpEvent{
		Type:        "error",
		ClusterID:   s.ID,
		Description: err.Error(),
	})
}

func (s *Cluster) handleDone() {
	s.sendEvent(&httpEvent{
		Type:      "install_done",
		ClusterID: s.ID,
		Cluster:   s,
	})
	s.installer.logger.Info(s.DashboardLoginMsg())
}

func (i *Installer) handleEvents() {
	for {
		select {
		case event := <-i.eventChan:
			i.logger.Info(event.Description)
			i.SendEvent(event)
		case err := <-i.errChan:
			i.logger.Info(err.Error())
			i.SendEvent(&httpEvent{
				Type:        "error",
				Description: err.Error(),
			})
		}
	}
}
