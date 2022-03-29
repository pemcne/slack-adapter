package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-joe/joe"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"
)

// EventsAPIServer is an adapter that receives messages from Slack using the events API.
// In contrast to the classical adapter, this server receives messages as HTTP
// requests instead of via a websocket.
//
// See https://api.slack.com/events-api
type EventsAPIServer struct {
	*BotAdapter
	http         *http.Server
	socketClient *socketmode.Client
	conf         EventsAPIConfig
	opts         []slackevents.Option
}

// EventsAPIAdapter returns a new EventsAPIServer as joe.Module.
// If you want to use the slack RTM API instead (i.e. using web sockets), you
// should use the slack.Adapter(…) function instead.
func EventsAPIAdapter(token string, opts ...Option) joe.Module {
	return joe.ModuleFunc(func(joeConf *joe.Config) error {
		conf, err := newConf(token, joeConf, opts)
		if err != nil {
			return err
		}
		if conf.EventsAPI.HTTPConfig.ListenAddr != "" {
			a, err := NewEventsAPIServer(joeConf.Context, conf)
			if err != nil {
				return err
			}
			joeConf.SetAdapter(a)
		} else if conf.EventsAPI.SocketConfig.AppToken != "" {
			a, err := NewEventsAPISocket(joeConf.Context, conf)
			if err != nil {
				return err
			}
			joeConf.SetAdapter(a)
		} else {
			return joe.Error("WithHTTPServer or WithSocketMode is required")
		}
		return nil
	})
}

// NewEventsAPIServer creates a new *EventsAPIServer that connects to Slack
// using the events API. Note that you will usually configure this type of slack
// adapter as joe.Module (i.e. using the EventsAPIAdapter function of this package).
func NewEventsAPIServer(ctx context.Context, conf Config) (*EventsAPIServer, error) {
	events := make(chan slackEvent)
	client := slack.New(conf.Token, conf.slackOptions()...)
	adapter, err := newAdapter(ctx, client, nil, events, conf)
	if err != nil {
		return nil, err
	}

	a := &EventsAPIServer{
		BotAdapter: adapter,
		conf:       conf.EventsAPI,
	}
	// Default back to HTTP server
	httpConfig := conf.EventsAPI.HTTPConfig
	a.opts = append(a.opts, slackevents.OptionVerifyToken(
		&slackevents.TokenComparator{
			VerificationToken: httpConfig.VerificationToken,
		},
	))

	var handler http.Handler = http.HandlerFunc(a.httpHandler)
	if httpConfig.Middleware != nil {
		handler = httpConfig.Middleware(handler)
	}

	a.http = &http.Server{
		Addr:         httpConfig.ListenAddr,
		Handler:      handler,
		ErrorLog:     zap.NewStdLog(conf.Logger),
		TLSConfig:    httpConfig.TLSConf,
		ReadTimeout:  httpConfig.ReadTimeout,
		WriteTimeout: httpConfig.WriteTimeout,
	}

	return a, nil
}

func NewEventsAPISocket(ctx context.Context, conf Config) (*EventsAPIServer, error) {
	events := make(chan slackEvent)
	slackOpts := conf.slackOptions()
	slackOpts = append(slackOpts, slack.OptionAppLevelToken(conf.EventsAPI.SocketConfig.AppToken), slack.OptionDebug(true))
	api := slack.New(conf.Token, slackOpts...)
	client := socketmode.New(api, socketmode.OptionDebug(conf.Debug))

	adapter, err := newAdapter(ctx, client, nil, events, conf)
	if err != nil {
		return nil, err
	}

	a := &EventsAPIServer{
		BotAdapter: adapter,
		conf:       conf.EventsAPI,
	}

	a.socketClient = client

	go func() {
		for evt := range client.Events {
			if evt.Type == socketmode.EventTypeEventsAPI {
				conf.Logger.Info("Got an event from socket")
				apiEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					conf.Logger.Info("Ignoring event")
					continue
				}
				client.Ack(*evt.Request)
				if apiEvent.Type == slackevents.CallbackEvent {
					conf.Logger.Info("Got a callback event")
					a.handleEvent(apiEvent.InnerEvent)
				}
			}
		}
	}()

	return a, nil
}

// RegisterAt implements the joe.Adapter interface by emitting the slack API
// events to the given brain.
func (a *EventsAPIServer) RegisterAt(brain *joe.Brain) {
	// Start the HTTP server. The goroutine will stop when the adapter is closed.
	if a.conf.HTTPConfig.ListenAddr != "" {
		go a.startHTTPServer()
	} else if a.conf.SocketConfig.AppToken != "" {
		go a.socketClient.RunContext(a.context)
	}
	a.BotAdapter.RegisterAt(brain)
}

func (a *EventsAPIServer) startHTTPServer() {
	var err error
	if a.conf.HTTPConfig.CertFile == "" {
		err = a.http.ListenAndServe()
	} else {
		err = a.http.ListenAndServeTLS(a.conf.HTTPConfig.CertFile, a.conf.HTTPConfig.KeyFile)
	}

	if err != nil && err != http.ErrServerClosed {
		a.logger.Error("HTTP server failure", zap.Error(err))
	}
}

func (a *EventsAPIServer) httpHandler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		a.logger.Error("Failed to read request body", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	eventsAPIEvent, err := slackevents.ParseEvent(body, a.opts...)
	if err != nil {
		a.logger.Error("Failed to parse slack event", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	switch eventsAPIEvent.Type {
	case slackevents.URLVerification:
		a.handleURLVerification(body, w)

	case slackevents.CallbackEvent:
		a.handleEvent(eventsAPIEvent.InnerEvent)

	default:
		a.logger.Error("Received unknown top level event type",
			zap.String("type", eventsAPIEvent.Type),
		)
	}
}

func (a *EventsAPIServer) handleURLVerification(req []byte, resp http.ResponseWriter) {
	a.logger.Info("Received URL verification challenge request")

	var r slackevents.ChallengeResponse
	err := json.Unmarshal(req, &r)
	if err != nil {
		a.logger.Error("Failed to unmarshal challenge as JSON", zap.Error(err))
		resp.WriteHeader(http.StatusInternalServerError)
		return
	}

	resp.Header().Set("Content-Type", "text")
	_, err = fmt.Fprint(resp, r.Challenge)
	if err != nil {
		a.logger.Error("Failed to write challenge response", zap.Error(err))
	}

	resp.WriteHeader(http.StatusOK)
}

func (a *EventsAPIServer) handleEvent(innerEvent slackevents.EventsAPIInnerEvent) {
	switch ev := innerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		a.handleMessageEvent(ev)

	case *slackevents.AppMentionEvent:
		a.handleAppMentionEvent(ev)

	case *slackevents.ReactionAddedEvent:
		a.handleReactionAddedEvent(ev)

	default:
		if a.logUnknownMessageTypes {
			a.logger.Error("Received unknown event type",
				zap.String("type", innerEvent.Type),
				zap.Any("data", innerEvent.Data),
				zap.String("go_type", fmt.Sprintf("%T", innerEvent.Data)),
			)
		}
	}
}

func (a *EventsAPIServer) handleMessageEvent(ev *slackevents.MessageEvent) {
	// Socket api gives you a message event and app mention event for each
	// mention so filter out an app mention here since it will be taken care
	// of by the next event
	selflink := a.userLink(a.userID)
	if strings.Contains(ev.Text, selflink) {
		return
	}
	var edited *slack.Edited
	if ev.Edited != nil {
		edited = &slack.Edited{
			User:      ev.Edited.User,
			Timestamp: ev.Edited.TimeStamp,
		}
	}

	icons := &slack.Icon{}
	if ev.Icons != nil {
		icons = &slack.Icon{
			IconURL:   ev.Icons.IconURL,
			IconEmoji: ev.Icons.IconEmoji,
		}
	}

	a.events <- slackEvent{
		Type: ev.Type,
		Data: &slack.MessageEvent{
			Msg: slack.Msg{
				Type:            ev.Type,
				Channel:         ev.Channel,
				User:            ev.User,
				Text:            ev.Text,
				Timestamp:       ev.TimeStamp,
				ThreadTimestamp: ev.ThreadTimeStamp,
				Edited:          edited,
				SubType:         ev.SubType,
				EventTimestamp:  ev.EventTimeStamp.String(),
				BotID:           ev.BotID,
				Username:        ev.Username,
				Icons:           icons,
			},
		},
	}
}

func (a *EventsAPIServer) handleAppMentionEvent(ev *slackevents.AppMentionEvent) {
	a.events <- slackEvent{
		Type: ev.Type,
		Data: &slack.MessageEvent{
			Msg: slack.Msg{
				Type:            ev.Type,
				User:            ev.User,
				Text:            ev.Text,
				Timestamp:       ev.TimeStamp,
				ThreadTimestamp: ev.ThreadTimeStamp,
				Channel:         ev.Channel,
				EventTimestamp:  ev.EventTimeStamp.String(),
				BotID:           ev.BotID,
			},
		},
	}
}

func (a *EventsAPIServer) handleReactionAddedEvent(ev *slackevents.ReactionAddedEvent) {
	evt := &slack.ReactionAddedEvent{
		Type:           ev.Type,
		User:           ev.User,
		ItemUser:       ev.ItemUser,
		Reaction:       ev.Reaction,
		EventTimestamp: ev.EventTimestamp,
	}

	evt.Item.Type = ev.Item.Type
	evt.Item.Channel = ev.Item.Channel
	evt.Item.Timestamp = ev.Item.Timestamp

	a.events <- slackEvent{
		Type: ev.Type,
		Data: evt,
	}
}

// Close shuts down the disconnects the adapter from the slack API.
func (a *EventsAPIServer) Close() error {
	ctx := context.Background()
	if a.conf.HTTPConfig.ListenAddr != "" {
		if a.conf.HTTPConfig.ShutdownTimeout > 0 {
			var cancel func()
			ctx, cancel = context.WithTimeout(ctx, a.conf.HTTPConfig.ShutdownTimeout)
			defer cancel()
		}

		err := a.http.Shutdown(ctx)
		return err
	}

	// After we are sure we do not get any new events from the HTTP server, we
	// must stop event processing loop by closing the channel.
	close(a.events)

	return nil
}
