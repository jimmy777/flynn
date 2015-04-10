package installer

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/flynn/flynn/pkg/random"
)

func (prompt *Prompt) Resolve(res *Prompt) {
	prompt.Resolved = true
	prompt.resChan <- res
}

func (event *Event) EventID() string {
	return event.ID
}

type Subscription struct {
	LastEventID string
	EventChan   chan *Event
	DoneChan    chan struct{}
}

func (sub *Subscription) SendEvents(i *Installer) {
	for _, event := range i.GetEventsSince(sub.LastEventID) {
		sub.LastEventID = event.ID
		sub.EventChan <- event
	}
}

func (i *Installer) Subscribe(eventChan chan *Event, lastEventID string) {
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

func (i *Installer) GetEventsSince(eventID string) []*Event {
	i.dbMtx.Lock()
	defer i.dbMtx.Unlock()
	events := make([]*Event, 0, len(i.events))
	var ts time.Time
	if eventID != "" {
		nano, err := strconv.ParseInt(strings.TrimPrefix(eventID, "event-"), 10, 64)
		if err != nil {
			i.logger.Debug(fmt.Sprintf("Error parsing event id: %s", err.Error()))
		} else {
			ts = time.Unix(0, nano)
		}
	}
	// TODO(jvatic): Convert the below queries to use ql
	rows, err := i.db.Query(`SELECT id, cluster, prompt, type, timestamp, description FROM events WHERE datetime(timestamp) >= datetime(?)`, ts.Truncate(time.Second).Format(time.RFC3339Nano))
	if err != nil {
		i.logger.Debug(fmt.Sprintf("GetEventsSince SQL Error: %s", err.Error()))
		return events
	}
	for rows.Next() {
		event := &Event{}
		var timestamp string
		if err := rows.Scan(&event.ID, &event.ClusterID, &event.PromptID, &event.Type, &timestamp, &event.Description); err != nil {
			i.logger.Debug(fmt.Sprintf("GetEventsSince Scan Error: %s", err.Error()))
			continue
		}
		event.Timestamp, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			i.logger.Debug("event timestamp is not parsable")
			continue
		}
		if !event.Timestamp.After(ts) {
			// sqlite compares at too low a resolution
			continue
		}
		if event.Type == "install_log" {
			if c, err := i.FindCluster(event.ClusterID); err != nil || (err == nil && c.State == "running") {
				continue
			}
		}
		if event.Type == "new_cluster" || event.Type == "install_done" {
			event.Cluster, err = i.FindCluster(event.ClusterID)
			if err != nil {
				i.logger.Debug(fmt.Sprintf("GetEventsSince Error finding cluster %s: %s", event.ClusterID, err.Error()))
				continue
			}
		}
		if event.PromptID != "" {
			p := &Prompt{}
			if err := i.db.QueryRow(`SELECT id, type, message, yes, input, resolved FROM prompts WHERE id = ? AND cluster = ?`, event.PromptID, event.ClusterID).Scan(&p.ID, &p.Type, &p.Message, &p.Yes, &p.Input, &p.Resolved); err != nil {
				i.logger.Debug(fmt.Sprintf("GetEventsSince Prompt Scan Error: %s", err.Error()))
				continue
			}
			event.Prompt = p
		}
		events = append(events, event)
	}
	return events
}

func (i *Installer) SendEvent(event *Event) {
	event.Timestamp = time.Now()
	event.ID = fmt.Sprintf("event-%d", event.Timestamp.UnixNano())

	if event.Type == "prompt" {
		if event.Prompt == nil {
			i.logger.Debug(fmt.Sprintf("SendEvent Error: Invalid prompt event: %v", event))
			return
		}
		event.PromptID = event.Prompt.ID
	}

	i.dbMtx.Lock()
	tx, err := i.db.Begin()
	if err != nil {
		i.logger.Debug(err.Error())
		tx.Rollback()
		i.dbMtx.Unlock()
		return
	}
	if _, err := tx.Exec(`INSERT INTO events (id, cluster, prompt, type, timestamp, description) VALUES (?, ?, ?, ?, ?, ?)`, event.ID, event.ClusterID, event.PromptID, event.Type, event.Timestamp.Format(time.RFC3339Nano), event.Description); err != nil {
		i.logger.Debug(fmt.Sprintf("SendEvent Error: %s", err.Error()))
		tx.Rollback()
		i.dbMtx.Unlock()
		return
	}
	tx.Commit()
	i.dbMtx.Unlock()

	i.logger.Info(fmt.Sprintf("Event: %s: %s", event.Type, event.Description))

	for _, sub := range i.subscriptions {
		go sub.SendEvents(i)
	}
}

func (c *Cluster) findPrompt(id string) (*Prompt, error) {
	if c.pendingPrompt != nil && c.pendingPrompt.ID == id {
		return c.pendingPrompt, nil
	}
	return nil, errors.New("Prompt not found")
}

func (c *Cluster) sendPrompt(prompt *Prompt) *Prompt {
	c.pendingPrompt = prompt

	c.installer.dbMtx.Lock()
	tx, err := c.installer.db.Begin()
	if err != nil {
		c.installer.logger.Debug("sendPrompt Begin error: %s", err.Error())
		tx.Rollback()
	} else {
		if _, err := tx.Exec(`INSERT INTO prompts (id, cluster, type, message, yes, input, resolved) VALUES (?, ?, ?, ?, ?, ?, ?)`, prompt.ID, c.ID, prompt.Type, prompt.Message, prompt.Yes, prompt.Input, prompt.Resolved); err != nil {
			c.installer.logger.Debug(fmt.Sprintf("sendPrompt SQL Error: %s", err.Error()))
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}
	c.installer.dbMtx.Unlock()

	c.sendEvent(&Event{
		Type:      "prompt",
		ClusterID: c.ID,
		Prompt:    prompt,
	})

	res := <-prompt.resChan
	prompt.Resolved = true

	tx, err = c.installer.db.Begin()
	if err != nil {
		c.installer.logger.Debug("sendPrompt res Begin error: %s", err.Error())
		tx.Rollback()
	} else {
		if _, err := tx.Exec(`UPDATE prompts SET resolved = ? WHERE id = ?`, prompt.Resolved, prompt.ID); err != nil {
			c.installer.logger.Debug(fmt.Sprintf("sendPrompt res SQL Error: %s", err.Error()))
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}

	c.sendEvent(&Event{
		Type:      "prompt",
		ClusterID: c.ID,
		Prompt:    prompt,
	})

	return res
}

func (c *Cluster) YesNoPrompt(msg string) bool {
	res := c.sendPrompt(&Prompt{
		ID:      random.Hex(16),
		Type:    "yes_no",
		Message: msg,
		resChan: make(chan *Prompt),
		cluster: c,
	})
	return res.Yes
}

func (c *Cluster) PromptInput(msg string) string {
	res := c.sendPrompt(&Prompt{
		ID:      random.Hex(16),
		Type:    "input",
		Message: msg,
		resChan: make(chan *Prompt),
		cluster: c,
	})
	return res.Input
}

func (c *Cluster) sendEvent(event *Event) {
	c.installer.SendEvent(event)
}

func (c *Cluster) SendInstallLogEvent(description string) {
	c.sendEvent(&Event{
		Type:        "install_log",
		ClusterID:   c.ID,
		Description: description,
	})
}

func (c *Cluster) SendError(err error) {
	c.sendEvent(&Event{
		Type:        "error",
		ClusterID:   c.ID,
		Description: err.Error(),
	})
}

func (c *Cluster) handleDone() {
	c.sendEvent(&Event{
		Type:      "install_done",
		ClusterID: c.ID,
		Cluster:   c,
	})
	c.installer.logger.Info(c.DashboardLoginMsg())
}
