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

func getLatestMessages(slackAPI *slack.Client, channelID, ts, botID string, maxTokens int) ([]gogpt.ChatCompletionMessage, error) {
	log.Println("getting replies", channelID, ts)
	replies, _, _, err := slackAPI.GetConversationReplies(&slack.GetConversationRepliesParameters{
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
	converted := convertConversation(replies, botID)
	return truncateMessages(converted, maxTokens), nil
}

func getMessage(slackAPI *slack.Client, channelID, ts string) (*slack.Message, error) {
	replies, _, _, err := slackAPI.GetConversationReplies(&slack.GetConversationRepliesParameters{
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

func handleOsieteAI(slackAPI *slack.Client, gptClient *gogpt.Client, ev *slackevents.ReactionAddedEvent, preludeMessage gogpt.ChatCompletionMessage, botID string) error {

	channelID := ev.Item.Channel
	srcMessage, err := getMessage(slackAPI, channelID, ev.Item.Timestamp)
	if err != nil {
		return fmt.Errorf("failed getting message: %v", err)
	}

	ts := FirstNonEmptyString(srcMessage.ThreadTimestamp, srcMessage.Timestamp)
	truncated, err := getLatestMessages(slackAPI, channelID, ts, botID, 3000)
	if err != nil {
		return fmt.Errorf("failed getting latest messages: %v", err)
	}
	// prepend prelude to truncated
	truncated = append([]gogpt.ChatCompletionMessage{preludeMessage}, truncated...)
	truncated = append(truncated, gogpt.ChatCompletionMessage{
		Role:    "user",
		Content: srcMessage.Text,
	})
	logMessages(truncated)
	completion, err := createChatCompletion(truncated, context.Background(), gptClient)
	if err != nil {
		return fmt.Errorf("failed creating chat completion: %v", err)
	}
	completion = fmt.Sprintf("<@%s>\n\n%s", ev.User, completion)
	_, _, err = slackAPI.PostMessage(channelID, slack.MsgOptionText(completion, false), slack.MsgOptionTS(ts))
	if err != nil {
		return fmt.Errorf("failed posting message: %v", err)
	}
	return nil
}

func handleMention(slackAPI *slack.Client, gptClient *gogpt.Client, ev *slackevents.AppMentionEvent, preludeMessage gogpt.ChatCompletionMessage, botID string) error {
	if ev.BotID == botID {
		return nil
	}
	ts := FirstNonEmptyString(ev.ThreadTimeStamp, ev.TimeStamp)
	truncated, err := getLatestMessages(slackAPI, ev.Channel, ts, botID, 3000)
	if err != nil {
		return fmt.Errorf("failed getting latest messages: %v", err)
	}
	// prepend prelude to truncated
	truncated = append([]gogpt.ChatCompletionMessage{preludeMessage}, truncated...)
	logMessages(truncated)
	completion, err := createChatCompletion(truncated, context.Background(), gptClient)
	if err != nil {
		return fmt.Errorf("failed creating chat completion: %v", err)
	}

	log.Printf("completion: %s", completion)

	_, _, err = slackAPI.PostMessage(ev.Channel, slack.MsgOptionText(completion, false), slack.MsgOptionTS(ts))
	if err != nil {
		return fmt.Errorf("failed posting message: %v", err)
	}
	return nil
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

	slackAPI := slack.New(
		botToken,
		slack.OptionDebug(false),
		slack.OptionLog(log.New(os.Stdout, "api: ", log.Lshortfile|log.LstdFlags)),
		slack.OptionAppLevelToken(slackAppToken),
	)

	slackClient := socketmode.New(
		slackAPI,
		socketmode.OptionDebug(false),
		socketmode.OptionLog(log.New(os.Stdout, "socketmode: ", log.Lshortfile|log.LstdFlags)),
	)

	gptClient := gogpt.NewClient(openaiAPIKey)

	authTestResponse, err := slackClient.AuthTest()
	if err != nil {
		panic(err)
	}

	go func() {
		for evt := range slackClient.Events {
			switch evt.Type {
			case socketmode.EventTypeConnecting:
				fmt.Println("Connecting to Slack with Socket Mode...")
			case socketmode.EventTypeConnectionError:
				fmt.Printf("Connection failed. Retrying later...")
			case socketmode.EventTypeConnected:
				fmt.Println("Connected to Slack with Socket Mode.")
			case socketmode.EventTypeEventsAPI:
				eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					fmt.Printf("Ignored %+v\n", evt)
					continue
				}
				fmt.Printf("Event received: %+v\n", eventsAPIEvent)
				slackClient.Ack(*evt.Request)
				switch eventsAPIEvent.Type {
				case slackevents.CallbackEvent:
					innerEvent := eventsAPIEvent.InnerEvent
					switch ev := innerEvent.Data.(type) {
					case *slackevents.AppMentionEvent:
						err = handleMention(slackAPI, gptClient, ev, preludeMessage, authTestResponse.BotID)
						if err != nil {
							log.Printf("failed handling mention: %v", err)
							continue
						}
					case *slackevents.ReactionAddedEvent:
						if ev.Item.Type == "message" && ev.Reaction == "osiete_ai" {
							err = handleOsieteAI(slackAPI, gptClient, ev, preludeMessage, authTestResponse.BotID)
							if err != nil {
								log.Printf("failed handling osiete_ai: %v", err)
								continue
							}
						}
					case *slackevents.MemberJoinedChannelEvent:
						fmt.Printf("user %q joined to channel %q", ev.User, ev.Channel)
					}
				default:
					slackClient.Debugf("unsupported Events API event received")
				}
			}
		}
	}()
	err = slackClient.Run()
	if err != nil {
		panic(fmt.Errorf("failed running slack client: %w", err))
	}
}

func createChatCompletion(messages []gogpt.ChatCompletionMessage, ctx context.Context, c *gogpt.Client) (string, error) {
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
