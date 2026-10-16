package messaging

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"junglegaming/challenge/internal/config"
)

var Module = fx.Module("sqs", fx.Provide(New))

var WorkersModule = fx.Module("sqs-workers", fx.Invoke(startWorkers))

type Client struct {
	api   *sqs.Client
	names []string
}

func New(lc fx.Lifecycle, c config.Config) *Client {
	client := &Client{names: append([]string(nil), c.QueueNames...)}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	httpClient := &http.Client{Transport: transport, Timeout: c.DependencyTimeout}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, c.DependencyTimeout)
			defer cancel()
			cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.AWSRegion), awsconfig.WithRetryMaxAttempts(2), awsconfig.WithHTTPClient(httpClient))
			if err != nil {
				transport.CloseIdleConnections()
				return fmt.Errorf("load AWS configuration")
			}
			client.api = sqs.NewFromConfig(cfg, func(o *sqs.Options) {
				if c.SQSEndpoint != "" {
					o.BaseEndpoint = aws.String(c.SQSEndpoint)
				}
			})
			if err := client.Check(ctx); err != nil {
				transport.CloseIdleConnections()
				return err
			}
			return nil
		},
		OnStop: func(context.Context) error { transport.CloseIdleConnections(); return nil },
	})
	return client
}

// Check verifies existence and access for every queue, including DLQ and output.
func (c *Client) Check(ctx context.Context) error {
	if c.api == nil {
		return fmt.Errorf("SQS has not started")
	}
	for _, name := range c.names {
		out, err := c.api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		if err != nil {
			return fmt.Errorf("SQS queue unavailable: %s", name)
		}
		if _, err := c.api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: out.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}}); err != nil {
			return fmt.Errorf("SQS queue inaccessible: %s", name)
		}
	}
	return nil
}

// QueueDepth reports the broker's approximate visible queue depth.
func (c *Client) QueueDepth(ctx context.Context, name string) (int64, error) {
	q, err := c.api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		return 0, err
	}
	a, err := c.api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: q.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages}})
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(a.Attributes["ApproximateNumberOfMessages"], 10, 64)
}
