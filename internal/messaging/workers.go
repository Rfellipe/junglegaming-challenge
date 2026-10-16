package messaging

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"junglegaming/challenge/internal/config"
	"junglegaming/challenge/internal/domain"
	"junglegaming/challenge/internal/wagering"
)

type InputEnvelope struct {
	MessageID  string    `json:"messageId"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurredAt"`
	Data       struct {
		wagering.Request
		IdempotencyKey string `json:"idempotencyKey"`
	} `json:"data"`
}

func DecodeInput(body string) (InputEnvelope, string, error) {
	var e InputEnvelope
	d := json.NewDecoder(bytes.NewBufferString(body))
	d.DisallowUnknownFields()
	if len(body) > 16384 {
		return e, "", errors.New("oversized message")
	}
	if err := d.Decode(&e); err != nil {
		return e, "", err
	}
	if d.Decode(new(any)) != io.EOF {
		return e, "", errors.New("trailing input")
	}
	if e.Type != "WagerTransactionRequested" || e.MessageID == "" || e.OccurredAt.IsZero() {
		return e, "", errors.New("invalid envelope")
	}
	if err := e.Data.Validate(e.Data.IdempotencyKey); err != nil {
		return e, "", err
	}
	// Canonical envelope hash detects changes in metadata, key and business content.
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(b)
	return e, hex.EncodeToString(sum[:]), nil
}

// Hooks allow an integration harness to stop a process at exact durable boundaries.
// The application provides none; there is no HTTP or environment activation path.
type Hooks struct {
	AfterCommit  func(context.Context)
	AfterClaim   func(context.Context, wagering.Publication)
	AfterPublish func(context.Context, wagering.Publication)
}
type workerParams struct {
	fx.In
	Hooks *Hooks `optional:"true"`
}

func startWorkers(lc fx.Lifecycle, c config.Config, client *Client, service *wagering.Service, logger *slog.Logger, params workerParams) {
	var cancel context.CancelFunc
	var done chan struct{}
	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error {
		input, err := client.api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(c.QueueNames[0])})
		if err != nil {
			return err
		}
		output, err := client.api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(c.QueueNames[2])})
		if err != nil {
			return err
		}
		workerCtx, stop := context.WithCancel(context.Background())
		cancel = stop
		done = make(chan struct{})
		consumerDone := make(chan struct{})
		go func() {
			defer close(consumerDone)
			for workerCtx.Err() == nil {
				receiveCtx, stop := context.WithTimeout(workerCtx, 12*time.Second)
				batch, err := client.api.ReceiveMessage(receiveCtx, &sqs.ReceiveMessageInput{QueueUrl: input.QueueUrl, MaxNumberOfMessages: 1, WaitTimeSeconds: 1, VisibilityTimeout: 30, MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount}})
				stop()
				if err != nil {
					if !pause(workerCtx, time.Second) {
						return
					}
					continue
				}
				for _, message := range batch.Messages {
					service.Metrics.Add("input_deliveries_total", 1)
					e, hash, err := DecodeInput(aws.ToString(message.Body))
					permanent := err != nil
					if err == nil {
						attempt, stop := context.WithTimeout(workerCtx, 5*time.Second)
						_, err = service.ProcessMessage(attempt, e.Data.Request, e.Data.IdempotencyKey, e.MessageID, hash)
						stop()
						permanent = errors.Is(err, wagering.ErrConflict) || errors.Is(err, domain.ErrInvalid) || errors.Is(err, wagering.ErrNotFound)
					}
					cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
					if err == nil {
						if params.Hooks != nil && params.Hooks.AfterCommit != nil {
							params.Hooks.AfterCommit(workerCtx)
						}
						_, err = client.api.DeleteMessage(cleanup, &sqs.DeleteMessageInput{QueueUrl: input.QueueUrl, ReceiptHandle: message.ReceiptHandle})
					} else {
						// Invalid/conflicting messages are retained for broker redrive after five receives.
						received, _ := strconv.Atoi(message.Attributes["ApproximateReceiveCount"])
						visibility := int32(1 << min(max(received, 1), 4))
						if permanent {
							visibility = 2
						}
						if workerCtx.Err() != nil {
							visibility = 0
						}
						_, _ = client.api.ChangeMessageVisibility(cleanup, &sqs.ChangeMessageVisibilityInput{QueueUrl: input.QueueUrl, ReceiptHandle: message.ReceiptHandle, VisibilityTimeout: visibility})
						service.Metrics.Add("input_retries_total", 1)
						if permanent {
							service.Metrics.Add("input_invalid_total", 1)
						}
						logger.Warn("input deferred", "errorClass", "processing", "messageId", e.MessageID, "walletId", e.Data.WalletID, "providerId", e.Data.ProviderID)
					}
					stop()
				}
			}
		}()
		go func() {
			defer close(done)
			defer func() { <-consumerDone }()
			for workerCtx.Err() == nil {
				attempt, stop := context.WithTimeout(workerCtx, 5*time.Second)
				p, err := service.ClaimEvent(attempt)
				if err == nil {
					if params.Hooks != nil && params.Hooks.AfterClaim != nil {
						params.Hooks.AfterClaim(workerCtx, p)
					}
					_, err = client.api.SendMessage(attempt, &sqs.SendMessageInput{QueueUrl: output.QueueUrl, MessageBody: aws.String(string(p.Payload)), MessageGroupId: aws.String(p.WalletID), MessageDeduplicationId: aws.String(p.EventID)})
					if err == nil && params.Hooks != nil && params.Hooks.AfterPublish != nil {
						params.Hooks.AfterPublish(workerCtx, p)
					}
					finish, finishCancel := context.WithTimeout(context.Background(), 2*time.Second)
					completeErr := service.CompleteEvent(finish, p, err == nil)
					finishCancel()
					if err != nil {
						service.Metrics.Add("outbox_retries_total", 1)
					}
					logger.Info("outbox publication", "eventId", p.EventID, "walletId", p.WalletID, "attempt", p.Attempts, "published", err == nil, "confirmed", completeErr == nil)
					if completeErr != nil {
						logger.Warn("outbox completion deferred")
					}
				}
				stop()
				if err != nil {
					if !pause(workerCtx, 250*time.Millisecond) {
						return
					}
				}
			}
		}()
		return nil
	}, OnStop: func(ctx context.Context) error {
		cancel()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
}
func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
