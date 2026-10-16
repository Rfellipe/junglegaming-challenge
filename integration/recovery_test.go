//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"junglegaming/challenge/internal/app"
	"junglegaming/challenge/internal/config"
	"junglegaming/challenge/internal/domain"
	"junglegaming/challenge/internal/httpapi"
	"junglegaming/challenge/internal/messaging"
	"junglegaming/challenge/internal/migration"
	"junglegaming/challenge/internal/wagering"
)

// This helper runs the real Fx application in a separate OS process. Hooks are
// supplied only by this test binary, never by application configuration.
func TestRecoveryProcess(t *testing.T) {
	raw := os.Getenv("JUNGLE_TEST_PROCESS")
	if raw == "" {
		t.Skip("subprocess helper")
	}
	var c config.Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("JUNGLE_TEST_BARRIER")
	barrier := func(ctx context.Context, phase string, p wagering.Publication) {
		armed, err := os.ReadFile(filepath.Join(dir, "armed"))
		if err != nil || string(armed) != phase {
			return
		}
		f, err := os.OpenFile(filepath.Join(dir, "reached"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		b, _ := json.Marshal(struct {
			PID     int
			EventID string
		}{os.Getpid(), p.EventID})
		_, _ = f.Write(b)
		_ = f.Close()
		<-ctx.Done()
	}
	hooks := &messaging.Hooks{AfterCommit: func(ctx context.Context) { barrier(ctx, "commit", wagering.Publication{}) }, AfterClaim: func(ctx context.Context, p wagering.Publication) { barrier(ctx, "claim", p) }, AfterPublish: func(ctx context.Context, p wagering.Publication) { barrier(ctx, "publish", p) }}
	var server *httpapi.Server
	a := fx.New(fx.NopLogger, fx.Supply(c, hooks), app.Modules(), fx.Populate(&server))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("JUNGLE_TEST_READY"), []byte("http://"+server.HTTP.Addr), 0600); err != nil {
		t.Fatal(err)
	}
	<-make(chan struct{})
}
func eventually(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition not reached within", timeout)
}

func TestThreeProcessRecovery(t *testing.T) {
	c := testConfig(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, c.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("recovery_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP DATABASE "+quoted+" WITH (FORCE)")
	u, _ := url.Parse(c.DatabaseURL)
	u.Path = "/" + name
	c.DatabaseURL = u.String()
	migrations, _ := filepath.Abs("../migrations")
	if err = migration.Run(c.DatabaseURL, migrations, "up"); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, c.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	service := wagering.New(func() *pgxpool.Pool { return pool })
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.AWSRegion))
	if err != nil {
		t.Fatal(err)
	}
	broker := sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(c.SQSEndpoint) })
	c.QueueNames = []string{name + "-input.fifo", name + "-dlq.fifo", name + "-events.fifo"}
	urls := make([]string, 3)
	for _, i := range []int{1, 0, 2} {
		attrs := map[string]string{"FifoQueue": "true", "ContentBasedDeduplication": "false", "VisibilityTimeout": "2"}
		if i == 0 {
			arn := "arn:aws:sqs:" + c.AWSRegion + ":000000000000:" + c.QueueNames[1]
			policy, _ := json.Marshal(map[string]string{"deadLetterTargetArn": arn, "maxReceiveCount": "5"})
			attrs["RedrivePolicy"] = string(policy)
		}
		out, e := broker.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(c.QueueNames[i]), Attributes: attrs})
		if e != nil {
			t.Fatal(e)
		}
		urls[i] = aws.ToString(out.QueueUrl)
		defer broker.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: out.QueueUrl})
	}
	dir := t.TempDir()
	var processes []*exec.Cmd
	processBases := map[int]string{}
	startProcess := func() *exec.Cmd {
		c.HTTPAddr = "127.0.0.1:0"
		raw, _ := json.Marshal(c)
		ready := filepath.Join(dir, wagering.ID())
		cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryProcess$", "-test.timeout=5m")
		cmd.Env = append(os.Environ(), "JUNGLE_TEST_PROCESS="+string(raw), "JUNGLE_TEST_BARRIER="+dir, "JUNGLE_TEST_READY="+ready)
		log, err := os.Create(filepath.Join(dir, filepath.Base(ready)+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout = log
		cmd.Stderr = log
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		processes = append(processes, cmd)
		eventually(t, 40*time.Second, func() bool { _, e := os.Stat(ready); return e == nil })
		_ = log.Close()
		address, _ := os.ReadFile(ready)
		processBases[cmd.Process.Pid] = string(address)
		return cmd
	}
	defer func() {
		for _, p := range processes {
			_ = p.Process.Kill()
			_ = p.Wait()
		}
	}()
	for i := 0; i < 3; i++ {
		startProcess()
	}
	// Discover distinct process ports from their ready files.
	bases := func() []string {
		entries, _ := os.ReadDir(dir)
		var out []string
		for _, e := range entries {
			if len(e.Name()) == 36 {
				b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
				out = append(out, string(b))
			}
		}
		return out
	}()
	bearer := token(t, c, "provider-a", "provider-a-local")
	internal := token(t, c, "internal-service", "internal-service-local")
	other := token(t, c, "provider-b", "provider-b-local")
	client := &http.Client{Timeout: 10 * time.Second}
	call := func(base, method, path, auth, key string, body any) (int, []byte, error) {
		b, _ := json.Marshal(body)
		req, e := http.NewRequest(method, base+path, bytes.NewReader(b))
		if e != nil {
			return 0, nil, e
		}
		req.Header.Set("Authorization", "Bearer "+auth)
		req.Header.Set("Idempotency-Key", key)
		res, e := client.Do(req)
		if e != nil {
			return 0, nil, e
		}
		defer res.Body.Close()
		var data bytes.Buffer
		_, e = data.ReadFrom(res.Body)
		return res.StatusCode, data.Bytes(), e
	}
	money := func(a string) domain.Money {
		m, e := domain.Parse(a, "BRL")
		if e != nil {
			t.Fatal(e)
		}
		return m
	}
	wallet := func() wagering.WalletView {
		w, e := service.CreateWallet(ctx, wagering.ID(), money("100.00"))
		if e != nil {
			t.Fatal(e)
		}
		return w
	}
	request := func(w wagering.WalletView, amount string) wagering.Request {
		return wagering.Request{ProviderID: "provider-a", ExternalID: wagering.ID(), PlayerID: w.PlayerID, WalletID: w.ID, RoundID: "round", GameID: "game", Kind: domain.Bet, Money: money(amount)}
	}
	assertWallet := func(w wagering.WalletView, balance int64, entries int) {
		t.Helper()
		rec, e := service.Reconcile(ctx, w.ID)
		if e != nil || !rec.Consistent || rec.StoredBalance.Minor() != balance || rec.CheckedEntries != int64(entries) {
			t.Fatalf("reconciliation %+v %v", rec, e)
		}
	}
	envelopeTime := time.Now().UTC()
	send := func(r wagering.Request, messageID string) {
		e := messaging.InputEnvelope{MessageID: messageID, Type: "WagerTransactionRequested", OccurredAt: envelopeTime}
		e.Data.Request = r
		e.Data.IdempotencyKey = r.ExternalID
		b, _ := json.Marshal(e)
		_, err := broker.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(urls[0]), MessageBody: aws.String(string(b)), MessageGroupId: aws.String(r.WalletID), MessageDeduplicationId: aws.String(wagering.ID())})
		if err != nil {
			t.Fatal(err)
		}
	}
	count := func(sql string, args ...any) int {
		var n int
		if e := pool.QueryRow(ctx, sql, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	t.Run("concurrent replay", func(t *testing.T) {
		w := wallet()
		r := request(w, "25.00")
		var wg sync.WaitGroup
		errs := make(chan string, 50)
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				status, b, e := call(bases[i%3], "POST", "/wagering/transactions", bearer, r.ExternalID, r)
				var got wagering.Result
				if e != nil || status != 200 || json.Unmarshal(b, &got) != nil || got.Balance.Minor() != 7500 || got.Status != domain.Processed {
					errs <- fmt.Sprintf("%d %s %v", status, b, e)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Error(e)
		}
		assertWallet(w, 7500, 2)
		got, _ := service.Transaction(ctx, r.ProviderID, r.ExternalID, true)
		status, _, e := call(bases[0], "GET", "/wagering/transactions/"+got.TransactionID, other, "", nil)
		if e != nil || status != 404 {
			t.Fatal("isolation", status, e)
		}
		for _, path := range []string{"/wagering/transactions/" + got.TransactionID, "/providers/provider-a/wagering/transactions/" + r.ExternalID} {
			status, body, err := call(bases[1], "GET", path, bearer, "", nil)
			var queried wagering.TransactionView
			if err != nil || status != 200 || json.Unmarshal(body, &queried) != nil || queried.TransactionID != got.TransactionID || queried.Balance.Minor() != 7500 {
				t.Fatal(status, string(body), err)
			}
		}
		status, _, _ = call(bases[0], "GET", "/providers/provider-b/wagering/transactions/"+r.ExternalID, bearer, "", nil)
		if status != 403 {
			t.Fatal(status)
		}
		status, _, _ = call(bases[0], "POST", "/wallets/"+w.ID+"/reconciliation", bearer, "", nil)
		if status != 403 {
			t.Fatal(status)
		}
		status, body, err := call(bases[0], "POST", "/wallets/"+w.ID+"/reconciliation", internal, "", nil)
		var reconciliation wagering.Reconciliation
		if err != nil || status != 200 || json.Unmarshal(body, &reconciliation) != nil || !reconciliation.Consistent || reconciliation.Difference.Minor() != 0 {
			t.Fatal(status, string(body), err)
		}
		status, _, _ = call(bases[0], "GET", "/wallets/"+w.ID+"/ledger", bearer, "", nil)
		if status != 403 {
			t.Fatal(status)
		}
		status, b, e := call(bases[0], "GET", "/wallets/"+w.ID+"/ledger?limit=1", internal, "", nil)
		if e != nil || status != 200 {
			t.Fatal(status, string(b), e)
		}
	})
	t.Run("competing funds", func(t *testing.T) {
		w := wallet()
		var wg sync.WaitGroup
		codes := make(chan int, 2)
		for i := 0; i < 2; i++ {
			r := request(w, "80.00")
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				status, _, _ := call(bases[i], "POST", "/wagering/transactions", bearer, r.ExternalID, r)
				codes <- status
			}(i)
		}
		wg.Wait()
		close(codes)
		sum := 0
		for code := range codes {
			sum += code
		}
		if sum != 622 {
			t.Fatal("expected 200 and 422", sum)
		}
		assertWallet(w, 2000, 2)
	})
	t.Run("wallet lock isolation", func(t *testing.T) {
		locked := wallet()
		free := wallet()
		tx, e := pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if _, e = tx.Exec(ctx, `SELECT 1 FROM wagering.wallets WHERE id=$1 FOR UPDATE`, locked.ID); e != nil {
			t.Fatal(e)
		}
		r := request(free, "1.00")
		status, b, e := call(bases[2], "POST", "/wagering/transactions", bearer, r.ExternalID, r)
		if e != nil || status != 200 {
			t.Fatal(status, string(b), e)
		}
		assertWallet(free, 9900, 2)
	})
	t.Run("HTTP SQS and real redelivery", func(t *testing.T) {
		w := wallet()
		r := request(w, "10.00")
		mid := wagering.ID()
		lock, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Rollback(ctx)
		if _, err = lock.Exec(ctx, `SELECT 1 FROM wagering.wallets WHERE id=$1 FOR UPDATE`, w.ID); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			status, b, e := call(bases[1], "POST", "/wagering/transactions", bearer, r.ExternalID, r)
			if e == nil && status != 200 {
				e = fmt.Errorf("HTTP %d: %s", status, b)
			}
			result <- e
		}()
		send(r, mid)
		// Both transports must enter processing and wait on the same lock
		// before release; scheduling cannot turn this into sequential delivery.
		eventually(t, 2*time.Second, func() bool {
			return count(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT id::text,player_id::text,currency,balance,version,created_at,updated_at FROM wagering.wallets WHERE id=$1 FOR UPDATE%'`) >= 2
		})
		if err = lock.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err = <-result; err != nil {
			t.Fatal(err)
		}
		eventually(t, 10*time.Second, func() bool {
			return count(`SELECT count(*) FROM wagering.inbox WHERE message_id=$1 AND completed_at IS NOT NULL`, mid) == 1
		})
		send(r, mid)
		eventually(t, 10*time.Second, func() bool { return count(`SELECT deliveries FROM wagering.inbox WHERE message_id=$1`, mid) == 2 })
		// A fresh broker deduplication ID forces a second receive. Durable inbox
		// count and ledger remain one even though the envelope was sent twice.
		eventually(t, 10*time.Second, func() bool {
			attrs, e := broker.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(urls[0]), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible}})
			return e == nil && attrs.Attributes["ApproximateNumberOfMessages"] == "0" && attrs.Attributes["ApproximateNumberOfMessagesNotVisible"] == "0"
		})
		assertWallet(w, 9000, 2)
	})
	// Drain opening events before arming publisher interruption barriers.
	waitOutbox := func() {
		eventually(t, 30*time.Second, func() bool { return count(`SELECT count(*) FROM wagering.outbox WHERE published_at IS NULL`) == 0 })
	}
	waitOutbox()
	killAt := func(phase string, trigger func()) string {
		_ = os.Remove(filepath.Join(dir, "reached"))
		if err := os.WriteFile(filepath.Join(dir, "armed"), []byte(phase), 0600); err != nil {
			t.Fatal(err)
		}
		trigger()
		var hit struct {
			PID     int
			EventID string
		}
		eventually(t, 15*time.Second, func() bool {
			b, e := os.ReadFile(filepath.Join(dir, "reached"))
			return e == nil && json.Unmarshal(b, &hit) == nil
		})
		_ = os.Remove(filepath.Join(dir, "armed"))
		for _, p := range processes {
			if p.Process.Pid == hit.PID {
				if e := p.Process.Kill(); e != nil {
					t.Fatal(e)
				}
			}
		}
		return hit.EventID
	}
	t.Run("consumer commit before delete", func(t *testing.T) {
		w := wallet()
		r := request(w, "5.00")
		mid := wagering.ID()
		killAt("commit", func() { send(r, mid) })
		if count(`SELECT count(*) FROM wagering.inbox WHERE message_id=$1 AND completed_at IS NOT NULL`, mid) != 1 {
			t.Fatal("commit missing")
		}
		assertWallet(w, 9500, 2)
		eventually(t, 40*time.Second, func() bool {
			attrs, e := broker.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(urls[0]), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessagesNotVisible}})
			return e == nil && attrs.Attributes["ApproximateNumberOfMessagesNotVisible"] == "0"
		})
		eventually(t, 40*time.Second, func() bool { return count(`SELECT deliveries FROM wagering.inbox WHERE message_id=$1`, mid) == 2 })
		assertWallet(w, 9500, 2)
		startProcess()
	})
	waitOutbox()
	assertOutput := func(eventID string) {
		eventually(t, 20*time.Second, func() bool {
			out, e := broker.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(urls[2]), MaxNumberOfMessages: 10, VisibilityTimeout: 1})
			if e != nil {
				return false
			}
			found := false
			for _, message := range out.Messages {
				var envelope struct {
					EventID string `json:"eventId"`
				}
				if json.Unmarshal([]byte(aws.ToString(message.Body)), &envelope) == nil && envelope.EventID == eventID {
					found = true
				}
				_, _ = broker.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(urls[2]), ReceiptHandle: message.ReceiptHandle})
			}
			return found
		})
	}
	for _, phase := range []string{"claim", "publish"} {
		t.Run("publisher "+phase, func(t *testing.T) {
			eventID := killAt(phase, func() { wallet() })
			eventually(t, 30*time.Second, func() bool {
				return count(`SELECT count(*) FROM wagering.outbox WHERE event_id=$1 AND published_at IS NOT NULL`, eventID) == 1
			})
			if count(`SELECT attempts FROM wagering.outbox WHERE event_id=$1`, eventID) < 2 {
				t.Fatal("lease was not recovered")
			}
			assertOutput(eventID)
			startProcess()
			waitOutbox()
		})
	}
	t.Run("broker outage preserves committed events", func(t *testing.T) {
		docker := func(args ...string) error { cmd := exec.Command("docker", args...); cmd.Dir = ".."; return cmd.Run() }
		cmd := exec.Command("docker", "compose", "ps", "-q", "localstack")
		cmd.Dir = ".."
		raw, e := cmd.Output()
		if e != nil {
			t.Fatal(e)
		}
		container := strings.TrimSpace(string(raw))
		if container == "" {
			t.Fatal("LocalStack Compose container missing")
		}
		if e = docker("pause", container); e != nil {
			t.Fatal(e)
		}
		defer docker("unpause", container)
		w := wallet()
		assertWallet(w, 10000, 1)
		eventually(t, 10*time.Second, func() bool {
			return count(`SELECT count(*) FROM wagering.outbox WHERE payload->'data'->>'walletId'=$1 AND attempts>0 AND published_at IS NULL`, w.ID) > 0
		})
		if e = docker("unpause", container); e != nil {
			t.Fatal(e)
		}
		waitOutbox()
	})
	t.Run("poison DLQ", func(t *testing.T) {
		_, e := broker.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(urls[0]), MessageBody: aws.String("invalid"), MessageGroupId: aws.String("poison"), MessageDeduplicationId: aws.String(wagering.ID())})
		if e != nil {
			t.Fatal(e)
		}
		eventually(t, 30*time.Second, func() bool {
			out, e := broker.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(urls[1]), MaxNumberOfMessages: 1})
			return e == nil && len(out.Messages) == 1
		})
	})
	t.Run("restart preserves results and references", func(t *testing.T) {
		w := wallet()
		ref := request(w, "5.00")
		refund := ref
		refund.ExternalID = wagering.ID()
		refund.Kind = domain.Refund
		refund.ReferenceExternalID = ref.ExternalID
		result, e := service.Process(ctx, refund, refund.ExternalID)
		if e != nil || result.Status != domain.PendingReference {
			t.Fatal(result, e)
		}
		for _, p := range processes {
			_ = p.Process.Kill()
		}
		for i := 0; i < 3; i++ {
			startProcess()
		}
		base := processBases[processes[len(processes)-1].Process.Pid]
		// Verify persisted results through a newly started HTTP process too.
		r, e := service.Process(ctx, ref, ref.ExternalID)
		if e != nil {
			t.Fatal(e)
		}
		again, e := service.Process(ctx, ref, ref.ExternalID)
		if e != nil || !again.IdempotentReplay || again.TransactionID != r.TransactionID {
			t.Fatal(again, e)
		}
		bearer = token(t, c, "provider-a", "provider-a-local")
		status, b, httpErr := call(base, "POST", "/wagering/transactions", bearer, ref.ExternalID, ref)
		var replay wagering.Result
		if httpErr != nil || status != 200 || json.Unmarshal(b, &replay) != nil || !replay.IdempotentReplay || replay.TransactionID != r.TransactionID {
			t.Fatal(status, string(b), httpErr)
		}
		eventually(t, 10*time.Second, func() bool {
			got, e := service.Transaction(ctx, refund.ProviderID, refund.ExternalID, true)
			return e == nil && got.Status == domain.Processed
		})
		assertWallet(w, 10000, 3)
	})
	waitOutbox()
	var discrepancies, pending, leased int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM wagering.wallets w WHERE w.balance::numeric <> (SELECT COALESCE(sum(CASE direction WHEN 'CREDIT' THEN amount::numeric ELSE -amount::numeric END),0) FROM wagering.ledger WHERE wallet_id=w.id)`).Scan(&discrepancies); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM wagering.transactions WHERE status IN ('PENDING','PENDING_REFERENCE')`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM wagering.outbox WHERE lease_token IS NOT NULL OR published_at IS NULL`).Scan(&leased); err != nil {
		t.Fatal(err)
	}
	if discrepancies != 0 || pending != 0 || leased != 0 {
		t.Fatalf("recovery left discrepancies=%d pending=%d outbox=%d", discrepancies, pending, leased)
	}
}
