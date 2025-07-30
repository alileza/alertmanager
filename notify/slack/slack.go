// Copyright 2019 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	commoncfg "github.com/prometheus/common/config"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/nflog"
	"github.com/prometheus/alertmanager/nflog/nflogpb"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
)

// https://api.slack.com/reference/messaging/attachments#legacy_fields - 1024, no units given, assuming runes or characters.
const maxTitleLenRunes = 1024

// Notifier implements a Notifier for Slack notifications.
type Notifier struct {
	conf    *config.SlackConfig
	tmpl    *template.Template
	logger  *slog.Logger
	client  *http.Client
	retrier *notify.Retrier

	postJSONFunc func(ctx context.Context, client *http.Client, url string, body io.Reader) (*http.Response, error)
}

// New returns a new Slack notification handler.
func New(c *config.SlackConfig, t *template.Template, l *slog.Logger, httpOpts ...commoncfg.HTTPClientOption) (*Notifier, error) {
	client, err := commoncfg.NewClientFromConfig(*c.HTTPConfig, "slack", httpOpts...)
	if err != nil {
		return nil, err
	}

	return &Notifier{
		conf:         c,
		tmpl:         t,
		logger:       l,
		client:       client,
		retrier:      &notify.Retrier{},
		postJSONFunc: notify.PostJSON,
	}, nil
}

// request is the request for sending a slack notification.
type request struct {
	Channel     string       `json:"channel,omitempty"`
	Username    string       `json:"username,omitempty"`
	IconEmoji   string       `json:"icon_emoji,omitempty"`
	IconURL     string       `json:"icon_url,omitempty"`
	LinkNames   bool         `json:"link_names,omitempty"`
	ThreadTS    string       `json:"thread_ts,omitempty"`
	Attachments []attachment `json:"attachments"`
}

// attachment is used to display a richly-formatted message block.
type attachment struct {
	Title      string               `json:"title,omitempty"`
	TitleLink  string               `json:"title_link,omitempty"`
	Pretext    string               `json:"pretext,omitempty"`
	Text       string               `json:"text"`
	Fallback   string               `json:"fallback"`
	CallbackID string               `json:"callback_id"`
	Fields     []config.SlackField  `json:"fields,omitempty"`
	Actions    []config.SlackAction `json:"actions,omitempty"`
	ImageURL   string               `json:"image_url,omitempty"`
	ThumbURL   string               `json:"thumb_url,omitempty"`
	Footer     string               `json:"footer"`
	Color      string               `json:"color,omitempty"`
	MrkdwnIn   []string             `json:"mrkdwn_in,omitempty"`
}

// Notify implements the Notifier interface.
func (n *Notifier) Notify(ctx context.Context, as ...*types.Alert) (bool, error) {
	var err error
	var (
		data     = notify.GetTemplateData(ctx, n.tmpl, as, n.logger)
		tmplText = notify.TmplText(n.tmpl, data, &err)
	)
	var markdownIn []string

	if len(n.conf.MrkdwnIn) == 0 {
		markdownIn = []string{"fallback", "pretext", "text"}
	} else {
		markdownIn = n.conf.MrkdwnIn
	}

	title, truncated := notify.TruncateInRunes(tmplText(n.conf.Title), maxTitleLenRunes)
	if truncated {
		key, err := notify.ExtractGroupKey(ctx)
		if err != nil {
			return false, err
		}
		n.logger.Warn("Truncated title", "key", key, "max_runes", maxTitleLenRunes)
	}
	att := &attachment{
		Title:      title,
		TitleLink:  tmplText(n.conf.TitleLink),
		Pretext:    tmplText(n.conf.Pretext),
		Text:       tmplText(n.conf.Text),
		Fallback:   tmplText(n.conf.Fallback),
		CallbackID: tmplText(n.conf.CallbackID),
		ImageURL:   tmplText(n.conf.ImageURL),
		ThumbURL:   tmplText(n.conf.ThumbURL),
		Footer:     tmplText(n.conf.Footer),
		Color:      tmplText(n.conf.Color),
		MrkdwnIn:   markdownIn,
	}

	numFields := len(n.conf.Fields)
	if numFields > 0 {
		fields := make([]config.SlackField, numFields)
		for index, field := range n.conf.Fields {
			// Check if short was defined for the field otherwise fallback to the global setting
			var short bool
			if field.Short != nil {
				short = *field.Short
			} else {
				short = n.conf.ShortFields
			}

			// Rebuild the field by executing any templates and setting the new value for short
			fields[index] = config.SlackField{
				Title: tmplText(field.Title),
				Value: tmplText(field.Value),
				Short: &short,
			}
		}
		att.Fields = fields
	}

	numActions := len(n.conf.Actions)
	if numActions > 0 {
		actions := make([]config.SlackAction, numActions)
		for index, action := range n.conf.Actions {
			slackAction := config.SlackAction{
				Type:  tmplText(action.Type),
				Text:  tmplText(action.Text),
				URL:   tmplText(action.URL),
				Style: tmplText(action.Style),
				Name:  tmplText(action.Name),
				Value: tmplText(action.Value),
			}

			if action.ConfirmField != nil {
				slackAction.ConfirmField = &config.SlackConfirmationField{
					Title:       tmplText(action.ConfirmField.Title),
					Text:        tmplText(action.ConfirmField.Text),
					OkText:      tmplText(action.ConfirmField.OkText),
					DismissText: tmplText(action.ConfirmField.DismissText),
				}
			}

			actions[index] = slackAction
		}
		att.Actions = actions
	}

	req := &request{
		Channel:     tmplText(n.conf.Channel),
		Username:    tmplText(n.conf.Username),
		IconEmoji:   tmplText(n.conf.IconEmoji),
		IconURL:     tmplText(n.conf.IconURL),
		LinkNames:   n.conf.LinkNames,
		Attachments: []attachment{*att},
	}

	// Handle threading if enabled
	if n.conf.UseThreading {
		groupKey, err := notify.ExtractGroupKey(ctx)
		if err == nil {
			// Query notification log for existing thread timestamp
			if notificationLog := notify.NotificationLogFromContext(ctx); notificationLog != nil {
				receiverName, _ := notify.ReceiverName(ctx)
				receiver := &nflogpb.Receiver{
					GroupName:   receiverName,
					Integration: "slack",
					Idx:         0, // Assuming single slack config per receiver
				}

				entries, err := notificationLog.Query(
					nflog.QGroupKey(string(groupKey)),
					nflog.QReceiver(receiver),
				)
				if err == nil && len(entries) > 0 {
					// Use the thread timestamp from the most recent entry
					latestEntry := entries[len(entries)-1]
					if latestEntry.SlackThreadTs != "" {
						req.ThreadTS = latestEntry.SlackThreadTs
						n.logger.Debug("Using existing thread for notification", "thread_ts", req.ThreadTS, "group_key", groupKey)
					}
				}
			}
		}
	}

	if err != nil {
		return false, err
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(req); err != nil {
		return false, err
	}

	var u string
	if n.conf.APIURL != nil {
		u = n.conf.APIURL.String()
	} else {
		content, err := os.ReadFile(n.conf.APIURLFile)
		if err != nil {
			return false, err
		}
		u = strings.TrimSpace(string(content))
	}

	resp, err := n.postJSONFunc(ctx, n.client, u, &buf)
	if err != nil {
		return true, notify.RedactURL(err)
	}
	defer notify.Drain(resp)

	// Use a retrier to generate an error message for non-200 responses and
	// classify them as retriable or not.
	retry, err := n.retrier.Check(resp.StatusCode, resp.Body)
	if err != nil {
		err = fmt.Errorf("channel %q: %w", req.Channel, err)
		return retry, notify.NewErrorWithReason(notify.GetFailureReasonFromStatusCode(resp.StatusCode), err)
	}

	// Slack web API might return errors with a 200 response code.
	// Also parse the response for thread timestamp if needed.
	// https://slack.dev/node-slack-sdk/web-api#handle-errors
	retry, body, err := n.checkResponseErrorAndParseBody(resp)
	if err != nil {
		err = fmt.Errorf("channel %q: %w", req.Channel, err)
		return retry, notify.NewErrorWithReason(notify.ClientErrorReason, err)
	}

	// Store thread timestamp in notification log if threading is enabled and this is a new thread
	if n.conf.UseThreading && req.ThreadTS == "" {
		if slackResp, err := parseSlackResponse(body); err == nil && slackResp.OK && slackResp.TS != "" {
			// Store thread timestamp in the container for SetNotifiesStage to use
			if container, ok := notify.SlackThreadTSContainerFromContext(ctx); ok {
				container.ThreadTS = slackResp.TS
				n.logger.Debug("New thread created, timestamp stored in container", "thread_ts", slackResp.TS)
			}
		}
	}

	return retry, nil
}

// checkResponseErrorAndParseBody parses out errors from Slack API response and returns the body.
func (n *Notifier) checkResponseErrorAndParseBody(resp *http.Response) (bool, []byte, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return true, nil, fmt.Errorf("could not read response body: %w", err)
	}

	var retry bool
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		retry, err = checkJSONResponseError(body)
	} else {
		retry, err = checkTextResponseError(body)
	}

	return retry, body, err
}

// checkTextResponseError classifies plaintext responses from Slack.
// A plaintext (non-JSON) response is successful if it's a string "ok".
// This is typically a response for an Incoming Webhook
// (https://api.slack.com/messaging/webhooks#handling_errors)
func checkTextResponseError(body []byte) (bool, error) {
	if !bytes.Equal(body, []byte("ok")) {
		return false, fmt.Errorf("received an error response from Slack: %s", string(body))
	}
	return false, nil
}

// checkJSONResponseError classifies JSON responses from Slack.
func checkJSONResponseError(body []byte) (bool, error) {
	// response is for parsing out errors from the JSON response.
	type response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}

	var data response
	if err := json.Unmarshal(body, &data); err != nil {
		return true, fmt.Errorf("could not unmarshal JSON response %q: %w", string(body), err)
	}
	if !data.OK {
		return false, fmt.Errorf("error response from Slack: %s", data.Error)
	}
	return false, nil
}

// slackResponse represents the successful response from Slack API
type slackResponse struct {
	OK      bool   `json:"ok"`
	TS      string `json:"ts"`      // Message timestamp
	Channel string `json:"channel"` // Channel ID
	Error   string `json:"error"`   // Error message if not OK
}

// parseSlackResponse extracts the timestamp from a successful Slack API response
func parseSlackResponse(body []byte) (*slackResponse, error) {
	var resp slackResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("could not unmarshal Slack response: %w", err)
	}
	return &resp, nil
}
