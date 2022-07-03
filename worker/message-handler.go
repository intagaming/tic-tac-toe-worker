package worker

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/ably/ably-go/ably"
)

type serverChannelCtxKey struct{}

type QueueMessage struct {
	Source  string `json:"source"`
	AppId   string `json:"appId"`
	Channel string `json:"channel"`
	Site    string `json:"site"`
	RuleId  string `json:"ruleId"`
}

type PresenceMessage struct {
	*QueueMessage
	Presence []Message `json:"presence"`
}

type MessageMessage struct {
	*QueueMessage
	Messages []Message `json:"messages"`
}

type Message struct {
	Id           string `json:"id"`
	ClientId     string `json:"clientId"`
	ConnectionId string `json:"connectionId"`
	Timestamp    int    `json:"timestamp"`
	Name         string `json:"name"`
	Action       int    `json:"action"`
	Data         string `json:"data"`
}

func unmarshalQueueMessage(payload []byte) (*QueueMessage, error) {
	msg := &QueueMessage{}
	err := json.Unmarshal(payload, msg)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

func unmarshalPresence(payload []byte) (*PresenceMessage, error) {
	msg := &PresenceMessage{}
	err := json.Unmarshal(payload, msg)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

func unmarshalMessage(payload []byte) (*MessageMessage, error) {
	msg := &MessageMessage{}
	err := json.Unmarshal(payload, msg)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

func handle(ctx context.Context, payload []byte) {
	queueMessage, err := unmarshalQueueMessage(payload)
	if err != nil {
		log.Println("Error unmarshalling queue message: ", err)
		return
	}

	// Attach "server:[roomId]" channel to context
	ablyClient := ctx.Value(ablyCtxKey{}).(*ably.Realtime)
	channel := queueMessage.Channel
	roomId := strings.Replace(channel, "control:", "", 1)
	serverChannel := ablyClient.Channels.Get("server:" + roomId)
	ctx = context.WithValue(ctx, serverChannelCtxKey{}, serverChannel)

	json := string(payload)
	if strings.Contains(json, "channel.presence") {
		msg, err := unmarshalPresence(payload)
		if err != nil {
			log.Println("Error unmarshalling presence message: ", err)
			return
		}
		handlePresence(ctx, msg)
	} else if strings.Contains(json, "channel.message") {
		msg, err := unmarshalMessage(payload)
		if err != nil {
			log.Println("Error unmarshalling message: ", err)
			return
		}
		handleMessage(ctx, msg)
	} else {
		log.Println("Unknown message: ", json)
	}
}

func handlePresence(ctx context.Context, presenceMsg *PresenceMessage) {
	msg := presenceMsg.Presence[0]
	// log.Println("Handling presence: ", msg)
	switch msg.Action {
	case int(ably.PresenceActionEnter):
		onEnter(ctx, presenceMsg)
	case int(ably.PresenceActionLeave):
		onLeave(ctx, presenceMsg)
	}
}

func handleMessage(ctx context.Context, messageMsg *MessageMessage) {
	msg := messageMsg.Messages[0]
	log.Println("Handling message: ", msg)
}
