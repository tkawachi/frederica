package main

import (
	"context"
	"fmt"
	"log"
	"os"

	gogpt "github.com/sashabaranov/go-gpt3"
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
	slackClient  *slack.Client
	socketClient *socketmode.Client
	gptClient    *gogpt.Client
	botID        string
	preludes     []gogpt.ChatCompletionMessage
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

func truncateMessages(messages []gogpt.ChatCompletionMessage, maxTokens int) []gogpt.ChatCompletionMessage {
	// keep latest messages to fit maxTokens
	var totalTokens int
	for i := len(messages) - 1; i >= 0; i-- {
		totalTokens += len(messages[i].Content)
		if totalTokens > maxTokens {
			return messages[i+1:]
		}
	}
	return messages
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
	return truncateMessages(converted, maxTokens), nil
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
	for _, msg := range messages {
		log.Printf("%s: %s", msg.Role, msg.Content)
	}
}

func (fred *Frederica) handleOsieteAI(ev *slackevents.ReactionAddedEvent) error {

	channelID := ev.Item.Channel
	srcMessage, err := fred.getMessage(channelID, ev.Item.Timestamp)
	if err != nil {
		return fmt.Errorf("failed getting message: %v", err)
	}

	ts := FirstNonEmptyString(srcMessage.ThreadTimestamp, srcMessage.Timestamp)
	truncated, err := fred.getLatestMessages(channelID, ts, 3000)
	if err != nil {
		return fmt.Errorf("failed getting latest messages: %v", err)
	}
	// prepend prelude to truncated
	truncated = append(fred.preludes, truncated...)
	truncated = append(truncated, gogpt.ChatCompletionMessage{
		Role:    "user",
		Content: srcMessage.Text,
	})
	logMessages(truncated)
	completion, err := createChatCompletion(context.Background(), truncated, fred.gptClient)
	if err != nil {
		return fmt.Errorf("failed creating chat completion: %v", err)
	}
	completion = fmt.Sprintf("<@%s>\n\n%s", ev.User, completion)
	_, _, err = fred.slackClient.PostMessage(channelID, slack.MsgOptionText(completion, false), slack.MsgOptionTS(ts))
	if err != nil {
		return fmt.Errorf("failed posting message: %v", err)
	}
	return nil
}

func (fred *Frederica) handleMention(ev *slackevents.AppMentionEvent) error {
	if ev.BotID == fred.botID {
		return nil
	}
	ts := FirstNonEmptyString(ev.ThreadTimeStamp, ev.TimeStamp)
	truncated, err := fred.getLatestMessages(ev.Channel, ts, 3000)
	if err != nil {
		return fmt.Errorf("failed getting latest messages: %v", err)
	}
	// prepend prelude to truncated
	truncated = append(fred.preludes, truncated...)
	logMessages(truncated)
	completion, err := createChatCompletion(context.Background(), truncated, fred.gptClient)
	if err != nil {
		return fmt.Errorf("failed creating chat completion: %v", err)
	}

	log.Printf("completion: %s", completion)

	_, _, err = fred.slackClient.PostMessage(ev.Channel, slack.MsgOptionText(completion, false), slack.MsgOptionTS(ts))
	if err != nil {
		return fmt.Errorf("failed posting message: %v", err)
	}
	return nil
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
			err := fred.handleMention(ev)
			if err != nil {
				return fmt.Errorf("failed handling mention: %v", err)
			}
		case *slackevents.ReactionAddedEvent:
			if ev.Item.Type == "message" && ev.Reaction == "osiete_ai" {
				err := fred.handleOsieteAI(ev)
				if err != nil {
					return fmt.Errorf("failed handling osiete_ai: %v", err)
				}
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

func main() {
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

	preludeMessage := gogpt.ChatCompletionMessage{
		Role:    "system",
		Content: "assistant の名前はフレデリカです",
	}

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
		slackClient:  slackClient,
		socketClient: socketClient,
		gptClient:    gptClient,
		botID:        authTestResponse.BotID,
		preludes:     []gogpt.ChatCompletionMessage{preludeMessage},
	}

	go fred.eventLoop()

	err = socketClient.Run()
	if err != nil {
		panic(fmt.Errorf("failed running slack client: %w", err))
	}
}

func createChatCompletion(ctx context.Context, messages []gogpt.ChatCompletionMessage, c *gogpt.Client) (string, error) {
	req := gogpt.ChatCompletionRequest{
		Model:     gogpt.GPT3Dot5Turbo,
		MaxTokens: 700,
		Messages:  messages,
	}
	resp, err := c.CreateChatCompletion(ctx, req)
	if err != nil {
		return "", fmt.Errorf("failed creating chat completion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no choices returned")
	}
	return resp.Choices[0].Message.Content, nil
}
