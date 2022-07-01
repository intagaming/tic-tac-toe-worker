package worker

import (
	"encoding/json"
	"log"
)

type queueMessage struct {
	Source   string    `json:"source"`
	AppId    string    `json:"appId"`
	Channel  string    `json:"channel"`
	Site     string    `json:"site"`
	RuleId   string    `json:"ruleId"`
	Messages []message `json:"messages"`
}

type message struct {
	Id           string `json:"id"`
	ClientId     string `json:"clientId"`
	ConnectionId string `json:"connectionId"`
	Timestamp    int    `json:"timestamp"`
	Name         string `json:"name"`
	Data         string `json:"data"`
}

func unmarshal(payload []byte) (*queueMessage, error) {
	queueMsg := &queueMessage{}
	err := json.Unmarshal(payload, queueMsg)
	if err != nil {
		return nil, err
	}
	return queueMsg, nil
}

func handle(payload []byte) {
	queueMsg, err := unmarshal(payload)
	if err != nil {
		log.Println("Error unmarshalling queue message: ", err)
		return
	}
	msg := queueMsg.Messages[0]

	log.Println("Handling message: ", msg)
}
