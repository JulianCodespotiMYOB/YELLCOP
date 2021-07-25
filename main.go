package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

type handler struct {
	verify    string
	threshold int
	warnings  []string
	failures  []string
	api       *slack.Client
}

func (h *handler) Invoke(ctx context.Context, b []byte) ([]byte, error) {
	var req events.APIGatewayProxyRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("failed to unmarshal payload: %w", err)
	}
	if req.Path != "/" {
		return asAGPR(fmt.Sprintf("unsupported path: %s", req.Path), 501)
	}
	if req.HTTPMethod != http.MethodPost {
		return asAGPR(fmt.Sprintf("%s method not supported", req.HTTPMethod), 405)
	}

	options := slackevents.OptionVerifyToken(&slackevents.TokenComparator{VerificationToken: h.verify})
	event, err := slackevents.ParseEvent(json.RawMessage(req.Body), options)
	if err != nil {
		return asAGPR(fmt.Sprintf("failed to parse message: %s", err), 500)
	}

	switch event.Type {
	case slackevents.URLVerification:
		var r *slackevents.ChallengeResponse
		if err = json.Unmarshal([]byte(req.Body), &r); err != nil {
			return asAGPR(fmt.Sprintf("failed to parse body: %s", err), 500)
		}
		return asAGPR(r.Challenge, 200)

	case slackevents.CallbackEvent:
		switch m := event.InnerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			if m.ChannelType == "channel" {
				out, kick := h.checkMessage(m.Text)
				if kick {
					h.api.KickUserFromConversation(m.Channel, m.User)
					log.Printf("kicking: %s", m.User)
				}
				if out != "" {
					h.api.PostMessage(m.Channel, slack.MsgOptionText(strings.ToUpper(strings.ReplaceAll(out, "{user}", m.User)), false))
				}
			}
		case *slackevents.MemberJoinedChannelEvent:
			//log.Printf("member joined")
		case *slackevents.AppMentionEvent:
		}
	default:
		return asAGPR(fmt.Sprintf("missing type implementation: %s", event.Type), 501)
	}
	return asAGPR("ok", 200)
}

func (h *handler) checkMessage(msg string) (string, bool) {
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
	for _, r := range s {
		if !unicode.IsUpper(r) && unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// asAGPR simplifies returning an APIGatewayProxyResponse inline.
func asAGPR(body string, code int) ([]byte, error) {
	var err error
	if code > 499 {
		err = fmt.Errorf(body)
	}
	b, _ := json.Marshal(events.APIGatewayProxyResponse{
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
	h := &handler{
		threshold: 2,
	}
	h.verify, err = ssmGet("/yellcop/tokens/slack/verification-token", true)
	if err != nil {
		log.Fatalf("failed to fetch verify: %s", err)
	}

	warnings, err := ssmGet("/yellcop/warnings", false)
	if err != nil {
		log.Printf("failed to fetch warnings: %s", err)
		warnings = "WARNING FOR <@{user}>!"
	}
	h.warnings = strings.Split(warnings, "|")

	failures, err := ssmGet("/yellcop/failures", false)
	if err != nil {
		log.Printf("failed to fetch failures: %s", err)
		failures = "KICKING <@{user}>!"
	}
	h.failures = strings.Split(failures, "|")

	token, err := ssmGet("/yellcop/tokens/slack/bot-token", true)
	if err != nil {
		log.Fatalf("failed to fetch token: %s", err)
	}
	h.api = slack.New(token)

	if threshold := os.Getenv("THRESHOLD"); threshold != "" {
		i, err := strconv.ParseInt(threshold, 10, 8)
		if err != nil {
			log.Printf("failed to parse threshold: %s", err)
		}
		h.threshold = int(i)
	}

	rand.Seed(time.Now().UnixNano())

	lambda.StartHandler(h)
}
