// Kick users that don't yell.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"math/rand"
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

var warningMessages = []string{
	"warning for <@{user}>",
	"careful <@{user}>!",
	"use your shift key <@{user}>, its not that hard",
	"only caps <@{user}>!",
	"i can't hear you <@{user}>!!",
	"this is sig-yelling <@{user}>, that ain't yelling!",
}

var failureMessages = []string{
	"kicking <@{user}>",
	"<@{user}> has crossed the line, pull the lever!",
	"by the power vested in me, i hereby kick <@{user}>",
	"<@{user}> is experiencing caps difficulties",
	"<@{user}> do no pass go, do not collect $200",
	"oops, I just kicked <@{user}>",
}

var inactiveMessages = []string{
	"<@{user}> has fallen asleep, kicking",
	"lurker detected, kicking <@{user}>",
	"are you still around <@{user}>? No? Bye, bye",
}

var (
	emojiRE = regexp.MustCompile(`\:[^:\s]+\:`)
	urlRE   = regexp.MustCompile(`https?://\w+`)
)

const welcomeMsg = "Welcome <@{user}>. If this is your first time here, please use all capitals for all messages. If you are returning, you know the rules"

type LambdaFunctionURLResponse struct {
	IsBase64Encoded bool              `json:"isBase64Encoded"`
	StatusCode      int               `json:"statusCode"`
	Headers         map[string]string `json:"headers,omitempty"`
	Body            string            `json:"body"`
}

type handler struct {
	botAPI  *slack.Client
	userAPI *slack.Client
	log     *zap.Logger

	// Verification token, stored in SSM
	verify string

	// Number of words to only get a warning
	threshold int

	// Period of inactivity before booting
	inactiveTime time.Duration

	chUsers []string

	// Messages
	msgWarnings []string
	msgKick     []string
	msgInactive []string
}

func (h *handler) Invoke(ctx context.Context, b []byte) ([]byte, error) {
	log.Println("Invoke function started")

	var req events.LambdaFunctionURLRequest
	if err := json.Unmarshal(b, &req); err != nil {
		log.Println("Error unmarshalling payload")
		return nil, fmt.Errorf("failed to unmarshal payload: %w", err)
	}
	log.Println("Payload unmarshalled successfully")

	// if req.Path != "/" {
	// 	return asAGPR(fmt.Sprintf("unsupported path: %s", req.Path), 501)
	// }

	log.Println("HTTP method check passed")

	options := slackevents.OptionVerifyToken(&slackevents.TokenComparator{VerificationToken: h.verify})
	log.Println("Verifying token" + h.verify)
	event, err := slackevents.ParseEvent(json.RawMessage(req.Body), options)

	switch event.Type {
	case slackevents.URLVerification:
		var r *slackevents.ChallengeResponse
		if err = json.Unmarshal([]byte(req.Body), &r); err != nil {
			log.Println("Error parsing body for URL verification")
			return asLFUR(fmt.Sprintf("failed to parse body: %s", err), 500)
		}
		log.Println("URL verification successful")
		return asLFUR(r.Challenge, 200)

	case slackevents.Cal	lbackEvent:
		switch m := event.InnerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			h.log.Debug(m.Text, zap.String("type", m.ChannelType), zap.String("user", m.User))
			log.Println("Handling message event")
			if m.ChannelType == "channel" {
				out, kick := h.checkMessage(m.Text)
				if kick {
					h.kickUser(m.Channel, m.User, out)
				} else if out != "" {
					h.postMessage(m.Channel, m.User, out)
				}
				if rand.Intn(23) == time.Now().Hour() {
					h.checkHistory(m.Channel)
				}
			}
		case *slackevents.MemberJoinedChannelEvent:
			h.log.Info("member joined", zap.String("user", m.User))
			h.postMessage(m.Channel, m.User, welcomeMsg, slack.MsgOptionPostEphemeral(m.User))
			log.Println("Handling member joined event")
		}
	default:
		log.Printf("Missing type implementation: %s", event.Type)
		return asLFUR(fmt.Sprintf("missing type implementation: %s", event.Type), 501)
	}
	log.Println("Invoke function completed successfully")
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
		return randomMessage(h.msgKick), true
	} else if count > 0 {
		return randomMessage(h.msgWarnings), false
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

func (h *handler) kickUser(chID, uID, msg string) {
	h.log.Info("kicking", zap.String("user", uID), zap.String("message", msg))
	if err := h.userAPI.KickUserFromConversation(chID, uID); err != nil {
		h.log.Error("failed to kick", zap.Error(err))
		return
	}
	h.postMessage(chID, uID, msg)
}

func (h *handler) postMessage(chID, uID, msg string, opts ...slack.MsgOption) {
	opts = append(opts, slack.MsgOptionText(strings.ToUpper(strings.ReplaceAll(msg, "{user}", uID)), false))
	if _, _, err := h.botAPI.PostMessage(chID, opts...); err != nil {
		h.log.Error("failed to post", zap.Error(err))
	}
}
func (h *handler) fetchChannelUsers(chID string) {
	var err error
	h.chUsers = make([]string, 0)
	hasMore := true
	cursor := ""
	for hasMore {
		var members []string
		members, cursor, err = h.botAPI.GetUsersInConversation(&slack.GetUsersInConversationParameters{
			ChannelID: chID,
			Cursor:    cursor,
			Limit:     200,
		})
		if err != nil {
			h.log.Error("failed to get users", zap.Error(err))
			return
		}
		hasMore = (cursor != "")
		h.chUsers = append(h.chUsers, members...)
	}

	h.log.Info("channel users fetched", zap.Int("count", len(h.chUsers)))
}

func (h *handler) checkHistory(chID string) {
	var err error
	startTime := time.Now().Add(-1 * h.inactiveTime)

	if len(h.chUsers) == 0 {
		h.fetchChannelUsers(chID)
	}

	// Select a victim at random
	rand.Shuffle(len(h.chUsers), func(i, j int) {
		h.chUsers[i], h.chUsers[j] = h.chUsers[j], h.chUsers[i]
	})
	uID := h.chUsers[0]
	info, err := h.botAPI.GetUserInfo(uID)
	if err != nil {
		h.log.Error("failed to get user info", zap.Error(err))
		return
	}

	if info.IsBot {
		h.log.Info("ignoring history for bot")
		return
	}

	// Count their messages since the inactivity time
	query := fmt.Sprintf("from:<@%s> in:%s after:%s", uID, chID, startTime.Format(time.RFC3339))
	resp, err := h.userAPI.SearchMessages(query, slack.NewSearchParameters())
	if err != nil {
		h.log.Error("failed to search messages", zap.Error(err))
		return
	}

	h.log.Warn("checked history", zap.String("user", uID), zap.Int("count", resp.TotalCount), zap.String("query", query))
	if resp.TotalCount == 0 {
		//h.kickUser(chID, uID, randomMessage(h.msgInactive))
		h.postMessage(chID, uID, "non-yelling lurker detected, warning <@{user}>")
	}
	return
}

// asLFUR simplifies returning an LambdaFunctionURLResponse inline.
func asLFUR(body interface{}, code int) ([]byte, error) {
	var err error
	if code > 499 {
		err = fmt.Errorf("%v", body)
	}

	bodyStr, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		return nil, marshalErr
	}

	response := LambdaFunctionURLResponse{
		IsBase64Encoded: false,
		StatusCode:      code,
		Headers: map[string]string{
			"Access-Control-Allow-Headers": "Content-Type",
			"Access-Control-Allow-Methods": "OPTIONS,POST,GET,PUT",
			"Access-Control-Allow-Origin":  "*",
		},
		Body: string(bodyStr),
	}

	b, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		return nil, marshalErr
	}

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

func randomMessage(haystack []string) string {
	return haystack[rand.Intn(len(haystack))]
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
		threshold:    1,
		inactiveTime: 30 * 24 * time.Hour,
		log:          logger,
	}

	if s := os.Getenv("INACTIVITY"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			h.inactiveTime = d
		}
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

	h.msgWarnings = warningMessages
	if w, err := ssmGet("/yellcop/warnings", false); err == nil {
		h.msgWarnings = append(h.msgWarnings, strings.Split(w, "|")...)
	}

	h.msgKick = failureMessages
	if f, err := ssmGet("/yellcop/failures", false); err == nil {
		h.msgKick = append(h.msgKick, strings.Split(f, "|")...)
	}

	h.msgInactive = inactiveMessages
	if f, err := ssmGet("/yellcop/inactive", false); err == nil {
		h.msgInactive = append(h.msgInactive, strings.Split(f, "|")...)
	}

	lambda.StartHandler(h)
}

func init() {
	rand.Seed(time.Now().UnixNano())
}
