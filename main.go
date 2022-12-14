// Kick users that don't yell.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
)

var skipMessages = []string{
	"departure cancelled",
	"departure removal report",
	"has joined the channel",
	"has joined the group",
	"has left the channel",
	"renamed the channel from",
	"set the channel purpose",
	"set the channel topic",
	"set the channel's purpose",
	"set the channel's topic",
	"set up a reminder",
	"started a call",
	"this content can't be displayed",
	"this message was deleted",
}

var (
	emojiRE = regexp.MustCompile(`\:[^:\s]+\:`)
	urlRE   = regexp.MustCompile(`https?://\w+`)
)

const welcomeMsg = "Welcome <@{user}>. If this is your first time here, please use all capitals for all messages. If you are returning, you know the rules"

type handler struct {
	botAPI  *slack.Client
	userAPI *slack.Client
	log     *zap.Logger

	// Verification token, stored in SSM
	verify string

	// Number of words to only get a warning
	threshold int

	// Warning messages
	warnings []string

	// Kick messages
	failures []string
}

func (h *handler) Invoke(ctx context.Context, b []byte) ([]byte, error) {
	var req events.LambdaFunctionURLRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("failed to unmarshal payload: %w", err)
	}
	// if req.Path != "/" {
	// 	return asAGPR(fmt.Sprintf("unsupported path: %s", req.Path), 501)
	// }
	if req.RequestContext.HTTP.Method != http.MethodPost {
		return asLFUR(fmt.Sprintf("%s method not supported", req.RequestContext.HTTP.Method), 405)
	}

	options := slackevents.OptionVerifyToken(&slackevents.TokenComparator{VerificationToken: h.verify})
	event, err := slackevents.ParseEvent(json.RawMessage(req.Body), options)
	if err != nil {
		return asLFUR(fmt.Sprintf("failed to parse message: %s", err), 500)
	}

	switch event.Type {
	case slackevents.URLVerification:
		var r *slackevents.ChallengeResponse
		if err = json.Unmarshal([]byte(req.Body), &r); err != nil {
			return asLFUR(fmt.Sprintf("failed to parse body: %s", err), 500)
		}
		return asLFUR(r.Challenge, 200)

	case slackevents.CallbackEvent:
		switch m := event.InnerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			h.log.Debug(m.Text, zap.String("type", m.ChannelType), zap.String("user", m.User))
			if m.ChannelType == "channel" {
				out, kick := h.checkMessage(m.Text)
				if kick {
					err := h.userAPI.KickUserFromConversation(m.Channel, m.User)
					if err != nil {
						h.log.Error("failed to kick", zap.Error(err))
					} else {
						h.log.Info("kicking", zap.String("user", m.User), zap.String("message", m.Text))
					}
				}
				if out != "" {
					if _, _, err := h.botAPI.PostMessage(
						m.Channel,
						slack.MsgOptionText(strings.ToUpper(strings.ReplaceAll(out, "{user}", m.User)), false),
					); err != nil {
						h.log.Error("failed to post", zap.Error(err))
					}
				}
			}
		case *slackevents.MemberJoinedChannelEvent:
			h.log.Debug("member joined", zap.String("user", m.User))
			if _, _, err := h.botAPI.PostMessage(
				m.Channel,
				slack.MsgOptionText(strings.ToUpper(strings.ReplaceAll(welcomeMsg, "{user}", m.User)), false),
				slack.MsgOptionPostEphemeral(m.User),
			); err != nil {
				h.log.Error("failed to post welcome message", zap.Error(err))
			}
		case *slackevents.AppMentionEvent:
		}
	default:
		return asLFUR(fmt.Sprintf("missing type implementation: %s", event.Type), 501)
	}
	return asLFUR("ok", 200)
}

func (h *handler) checkMessage(msg string) (string, bool) {
	// Skip slack messages
	for _, m := range skipMessages {
		if strings.Contains(msg, m) {
			return "", false
		}
	}
	var count int
	for _, word := range strings.Fields(msg) {
		if !isYell(word) {
			count++
		}
	}
	if count > h.threshold {
		return h.failures[rand.Intn(len(h.failures))], true
	} else if count > 0 {
		return h.warnings[rand.Intn(len(h.warnings))], false
	}
	return "", false
}

func isYell(s string) bool {
	// Remove emojis first
	s = emojiRE.ReplaceAllString(s, "")
	if urlRE.MatchString(s) {
		return true
	}
	s = html.UnescapeString(s)
	return strings.ToUpper(s) == s
}

// asLFUR simplifies returning an LambdaFunctionURLResponse inline.
func asLFUR(body string, code int) ([]byte, error) {
	var err error
	if code > 499 {
		err = fmt.Errorf(body)
	}
	b, _ := json.Marshal(events.LambdaFunctionURLResponse{
		// Headers: map[string]string{
		// 	"Access-Control-Allow-Headers": "Content-Type",
		// 	"Access-Control-Allow-Methods": "OPTIONS,POST,GET,PUT",
		// 	"Access-Control-Allow-Origin":  "*",
		// },
		Body:       body,
		StatusCode: code,
	})
	return b, err
}

func ssmGet(key string, decrypt bool) (string, error) {
	if key == "" {
		return "", fmt.Errorf("ssm: missing key")
	}
	sess, err := session.NewSessionWithOptions(session.Options{
		Config: aws.Config{Region: aws.String("ap-southeast-2")},
	})
	if err != nil {
		return "", err
	}
	svc := ssm.New(sess, nil)

	// pull parameter.
	param, err := svc.GetParameter(&ssm.GetParameterInput{
		Name:           aws.String(key),
		WithDecryption: &decrypt,
	})
	if err != nil {
		return "", fmt.Errorf("ssm: failed to get config from paramstore: %s", err)
	}
	return *param.Parameter.Value, nil
}

func main() {
	var err error

	logCfg := zap.NewDevelopmentConfig()
	logCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	if os.Getenv("VERBOSE") != "" {
		logCfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	}
	logger, _ := logCfg.Build()

	h := &handler{
		threshold: 1,
		log:       logger,
	}

	h.verify, err = ssmGet("/yellcop/tokens/slack/verification-token", true)
	if err != nil {
		logger.Fatal("failed to fetch verify", zap.Error(err))
	}

	botToken, err := ssmGet("/yellcop/tokens/slack/bot-token", true)
	if err != nil {
		logger.Fatal("failed to fetch bot token", zap.Error(err))
	}
	h.botAPI = slack.New(botToken)

	userToken, err := ssmGet("/yellcop/tokens/slack/user-token", true)
	if err != nil {
		logger.Fatal("failed to fetch user token", zap.Error(err))
	}
	h.userAPI = slack.New(userToken)

	h.warnings = []string{
		"warning for <@{user}>",
		"careful <@{user}>!",
		"use your shift key <@{user}>, its not that hard",
		"only caps <@{user}>!",
		"i can't hear you <@{user}>!!",
		"this is sig-yelling <@{user}>, that ain't yelling!",
	}
	if w, err := ssmGet("/yellcop/warnings", false); err == nil {
		h.warnings = append(h.warnings, strings.Split(w, "|")...)
	}

	h.failures = []string{
		"kicking <@{user}>",
		"<@{user}> has crossed the line, pull the lever!",
		"by the power vested in me, i hereby kick <@{user}>",
		"<@{user}> is experiencing caps difficulties",
		"<@{user}> do no pass go, do not collect $200",
		"oops, I just kicked <@{user}>",
	}
	if f, err := ssmGet("/yellcop/failures", false); err == nil {
		h.failures = append(h.failures, strings.Split(f, "|")...)
	}

	lambda.StartHandler(h)
}

func init() {
	rand.Seed(time.Now().UnixNano())
}
