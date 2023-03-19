package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"

	tokenizer "github.com/samber/go-gpt-3-encoder"
	gogpt "github.com/sashabaranov/go-openai"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

func FirstNonEmptyString(strings ...string) string {
	for _, s := range strings {
		if s != "" {
			return s
		}
	}
	return ""
}

type Frederica struct {
	slackClient    *slack.Client
	socketClient   *socketmode.Client
	gptClient      *gogpt.Client
	gptModel       string
	gptTemperature float32
	gptMaxTokens   int
	gptEncoder     *tokenizer.Encoder
	botID          string
	botUserID      string
	preludes       []gogpt.ChatCompletionMessage
}

func convertConversation(messages []slack.Message, botID string) []gogpt.ChatCompletionMessage {
	var conversation []gogpt.ChatCompletionMessage
	for _, msg := range messages {
		if msg.User == "" || msg.Text == "" {
			continue
		}
		if msg.BotID == botID {
			conversation = append(conversation, gogpt.ChatCompletionMessage{
				Role:    "assistant",
				Content: msg.Text,
			})
		} else {
			conversation = append(conversation, gogpt.ChatCompletionMessage{
				Role:    "user",
				Content: msg.Text,
				// Name:    msg.User, // 今は Name がサポートされていない
			})
		}
	}
	return conversation
}

func (fred *Frederica) truncateMessages(messages []gogpt.ChatCompletionMessage, maxTokens int) ([]gogpt.ChatCompletionMessage, error) {
	// keep latest messages to fit maxTokens
	var totalTokens int
	for i := len(messages) - 1; i >= 0; i-- {
		content := messages[i].Content
		encoded, err := fred.gptEncoder.Encode(content)
		if err != nil {
			return nil, fmt.Errorf("failed encoding message %s: %v", content, err)
		}
		totalTokens += len(encoded)
		if totalTokens > maxTokens {
			return messages[i+1:], nil
		}
	}
	return messages, nil
}

func (fred *Frederica) getLatestMessages(channelID, ts string, maxTokens int) ([]gogpt.ChatCompletionMessage, error) {
	log.Println("getting replies", channelID, ts)
	replies, _, _, err := fred.slackClient.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: ts,
	})
	if err != nil {
		return nil, fmt.Errorf("failed getting conversation history: %v", err)
	}
	if len(replies) == 0 {
		return nil, fmt.Errorf("failed getting conversation history: no messages")
	}
	log.Println("got replies", len(replies))
	for _, msg := range replies {
		log.Printf("%s: %s %v %v", msg.User, msg.Text, msg.ThreadTimestamp, msg.Timestamp)
	}
	converted := convertConversation(replies, fred.botID)
	return fred.truncateMessages(converted, maxTokens)
}

func (fred *Frederica) getMessage(channelID, ts string) (*slack.Message, error) {
	replies, _, _, err := fred.slackClient.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: ts,
		Limit:     1,
	})
	if err != nil {
		return nil, fmt.Errorf("failed getting conversation history: %v", err)
	}
	if len(replies) == 0 {
		return nil, fmt.Errorf("failed getting conversation history: no messages")
	}
	return &replies[0], nil
}

func logMessages(messages []gogpt.ChatCompletionMessage) {
	log.Println("-----MESSAGES_BEGIN-----")
	for _, msg := range messages {
		log.Printf("%s: %s", msg.Role, msg.Content)
	}
	log.Println("-----MESSAGES_END-----")
}

func (fred *Frederica) handleOsieteAI(ev *slackevents.ReactionAddedEvent) {

	channelID := ev.Item.Channel
	srcMessage, err := fred.getMessage(channelID, ev.Item.Timestamp)
	if err != nil {
		log.Printf("ERROR: failed getting message: %v\n", err)
		return
	}

	ts := FirstNonEmptyString(srcMessage.ThreadTimestamp, srcMessage.Timestamp)
	truncated, err := fred.getLatestMessages(channelID, ts, 3000)
	if err != nil {
		log.Printf("ERROR: failed getting latest messages: %v\n", err)
		return
	}
	// prepend prelude to truncated
	truncated = append(fred.preludes, truncated...)
	// append reaction message if it's not located at the end
	if len(truncated) == 0 || truncated[len(truncated)-1].Content != srcMessage.Text {
		truncated = append(truncated, gogpt.ChatCompletionMessage{
			Role:    "user",
			Content: srcMessage.Text,
		})
	}
	logMessages(truncated)
	completion, err := fred.createChatCompletion(context.Background(), truncated)
	if err != nil {
		traceID := generateTraceID()
		fred.postErrorMessage(channelID, ts, traceID)
		log.Printf("ERROR: failed creating chat completion %s: %v\n", traceID, err)
		return
	}
	completion = fmt.Sprintf("<@%s>\n\n%s", ev.User, completion)
	err = fred.postOnThread(channelID, completion, ts)
	if err != nil {
		log.Printf("ERROR: failed posting message: %v\n", err)
		return
	}
}

func (fred *Frederica) postOnThread(channelID, message, ts string) error {
	_, _, err := fred.slackClient.PostMessage(channelID, slack.MsgOptionText(message, false), slack.MsgOptionTS(ts))
	if err != nil {
		return fmt.Errorf("failed posting message: %v", err)
	}
	return nil
}

// エラーを追跡するための ID を生成する。
// この ID はエラーが発生したときにユーザーに伝える。
func generateTraceID() string {
	// ランダムな6文字の文字列を生成
	n := 6
	letters := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	b := make([]rune, n)
	for i := range b {
		r, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		if err != nil {
			return string(b)
		}
		b[i] = letters[r.Int64()]
	}
	return string(b)
}

// postErrorMessage posts an error message on the thread
func (fred *Frederica) postErrorMessage(channelID, ts string, traceID string) {
	message := fmt.Sprintf("エラーが発生しました。また後で試してね。 %s", traceID)
	err := fred.postOnThread(channelID, message, ts)
	if err != nil {
		log.Printf("failed to access OpenAI API: %v\n", err)
	}
}

func (fred *Frederica) handleMention(ev *slackevents.AppMentionEvent) {
	if ev.BotID == fred.botID || ev.User == fred.botUserID {
		return
	}
	ts := FirstNonEmptyString(ev.ThreadTimeStamp, ev.TimeStamp)
	truncated, err := fred.getLatestMessages(ev.Channel, ts, 3000)
	if err != nil {
		log.Printf("ERROR: failed getting latest messages: %v\n", err)
		return
	}
	// prepend prelude to truncated
	truncated = append(fred.preludes, truncated...)
	logMessages(truncated)
	completion, err := fred.createChatCompletion(context.Background(), truncated)
	if err != nil {
		traceID := generateTraceID()
		fred.postErrorMessage(ev.Channel, ts, traceID)
		log.Printf("ERROR: failed creating chat completion %s: %v\n", traceID, err)
		return
	}
	err = fred.postOnThread(ev.Channel, completion, ts)
	if err != nil {
		log.Printf("ERROR: failed posting message: %v\n", err)
		return
	}
}

func (fred *Frederica) handleEventTypeEventsAPI(evt *socketmode.Event) error {
	eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		log.Printf("Ignored %+v\n", evt)
		return nil
	}
	log.Printf("Event received: %+v\n", eventsAPIEvent)
	fred.socketClient.Ack(*evt.Request)
	switch eventsAPIEvent.Type {
	case slackevents.CallbackEvent:
		innerEvent := eventsAPIEvent.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
			go fred.handleMention(ev)
		case *slackevents.ReactionAddedEvent:
			if ev.Item.Type == "message" && ev.Reaction == "osiete_ai" {
				go fred.handleOsieteAI(ev)
			}
		case *slackevents.MemberJoinedChannelEvent:
			fmt.Printf("user %q joined to channel %q", ev.User, ev.Channel)
		}
	default:
		fred.socketClient.Debugf("unsupported Events API event received")
	}
	return nil
}

func (fred *Frederica) eventLoop() {
	for evt := range fred.socketClient.Events {
		switch evt.Type {
		case socketmode.EventTypeConnecting:
			log.Println("Connecting to Slack with Socket Mode...")
		case socketmode.EventTypeConnectionError:
			log.Println("Connection failed. Retrying later...")
		case socketmode.EventTypeConnected:
			log.Println("Connected to Slack with Socket Mode.")
		case socketmode.EventTypeEventsAPI:
			err := fred.handleEventTypeEventsAPI(&evt)
			if err != nil {
				log.Printf("failed handling event type events api: %v\n", err)
				continue
			}
		}
	}
}

func getEnvInt(key string, defaultValue int) (int, error) {
	value, found := os.LookupEnv(key)
	if !found {
		return defaultValue, nil
	}
	return strconv.Atoi(value)
}

func getEnvFloat32(key string, defaultValue float32) (float32, error) {
	value, found := os.LookupEnv(key)
	if !found {
		return defaultValue, nil
	}
	f, err := strconv.ParseFloat(value, 32)
	if err != nil {
		return 0, err
	}
	return float32(f), nil
}

func main() {
	gptEncoder, err := tokenizer.NewEncoder()
	if err != nil {
		panic(err)
	}

	// read from environmental variable
	openaiAPIKey := os.Getenv("OPENAI_API_KEY")
	if openaiAPIKey == "" {
		panic("OPENAI_API_KEY is not set")
	}

	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		panic("BOT_TOKEN is not set")
	}

	slackAppToken := os.Getenv("SLACK_APP_TOKEN")
	if slackAppToken == "" {
		panic("SLACK_APP_TOKEN is not set")
	}

	gptModel, found := os.LookupEnv("GPT_MODEL")
	if !found {
		gptModel = gogpt.GPT4
	}
	log.Println("GPT_MODEL:", gptModel)

	gptTemperature, err := getEnvFloat32("GPT_TEMPERATURE", 0.5)
	if err != nil {
		panic(err)
	}
	gptMaxTokens, err := getEnvInt("GPT_MAX_TOKENS", 700)
	if err != nil {
		panic(err)
	}

	systemMessage, found := os.LookupEnv("SYSTEM_MESSAGE")
	if !found {
		systemMessage = "assistant の名前はフレデリカです"
	}

	preludeMessage := gogpt.ChatCompletionMessage{Role: "system", Content: systemMessage}

	slackClient := slack.New(
		botToken,
		slack.OptionDebug(false),
		slack.OptionLog(log.New(os.Stdout, "api: ", log.Lshortfile|log.LstdFlags)),
		slack.OptionAppLevelToken(slackAppToken),
	)

	socketClient := socketmode.New(
		slackClient,
		socketmode.OptionDebug(false),
		socketmode.OptionLog(log.New(os.Stdout, "socketmode: ", log.Lshortfile|log.LstdFlags)),
	)

	gptClient := gogpt.NewClient(openaiAPIKey)

	authTestResponse, err := slackClient.AuthTest()
	if err != nil {
		panic(err)
	}
	fred := &Frederica{
		slackClient:    slackClient,
		socketClient:   socketClient,
		gptClient:      gptClient,
		gptModel:       gptModel,
		gptEncoder:     gptEncoder,
		gptTemperature: gptTemperature,
		gptMaxTokens:   gptMaxTokens,
		botID:          authTestResponse.BotID,
		botUserID:      authTestResponse.UserID,
		preludes:       []gogpt.ChatCompletionMessage{preludeMessage},
	}

	go fred.eventLoop()

	err = socketClient.Run()
	if err != nil {
		panic(fmt.Errorf("failed running slack client: %w", err))
	}
}

func (fred *Frederica) createChatCompletion(ctx context.Context, messages []gogpt.ChatCompletionMessage) (string, error) {
	req := gogpt.ChatCompletionRequest{
		Model:       fred.gptModel,
		MaxTokens:   fred.gptMaxTokens,
		Temperature: fred.gptTemperature,
		Messages:    messages,
	}
	resp, err := fred.gptClient.CreateChatCompletion(ctx, req)
	if err != nil {
		return "", fmt.Errorf("failed creating chat completion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no choices returned")
	}
	completion := resp.Choices[0].Message.Content
	log.Printf("completion: %s\n", completion)
	return completion, nil
}
