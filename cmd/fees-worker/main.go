package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"pave-assignment/billing/workflowdef"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

func main() {
	hostPort := os.Getenv("TEMPORAL_ADDRESS")
	if hostPort == "" {
		hostPort = "localhost:7233"
	}
	namespace := os.Getenv("TEMPORAL_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL environment variable is required (e.g. run: encore db conn-uri billing)")
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("unable to open database: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Fatalf("unable to ping database: %v", err)
	}
	log.Println("database connection established")

	var c client.Client
	for attempt := 1; attempt <= 12; attempt++ {
		c, err = client.Dial(client.Options{
			HostPort:  hostPort,
			Namespace: namespace,
		})
		if err == nil {
			break
		}
		log.Printf("temporal dial attempt %d failed: %v", attempt, err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatalf("unable to create temporal client after retries: %v", err)
	}
	defer c.Close()

	// Use the shared BillCloser to avoid duplicating close-bill SQL across processes.
	closer := &pgBillCloser{db: db}
	activities := workflowdef.NewActivities(
		func(ctx context.Context, billID string) error {
			return workflowdef.CloseBillInDB(ctx, billID, closer)
		},
		sendInvoiceEmailStub,
	)

	w := worker.New(c, workflowdef.BillingTaskQueue, worker.Options{})
	w.RegisterWorkflowWithOptions(workflowdef.BillingWorkflow, workflow.RegisterOptions{
		Name: workflowdef.WorkflowName,
	})
	w.RegisterActivityWithOptions(activities.MarkClosedInDB, activity.RegisterOptions{
		Name: workflowdef.MarkClosedActivityName,
	})
	w.RegisterActivityWithOptions(activities.SendInvoiceEmail, activity.RegisterOptions{
		Name: workflowdef.SendInvoiceActivityName,
	})

	log.Println("starting billing worker")
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("unable to run worker: %v", err)
	}
}

// ---------------------------------------------------------------------------
// pgBillCloser implements workflowdef.BillCloser using database/sql.
// ---------------------------------------------------------------------------

type pgBillCloser struct {
	db *sql.DB
}

func (c *pgBillCloser) ExecCloseBill(ctx context.Context, billID string) (int64, error) {
	result, err := c.db.ExecContext(ctx, workflowdef.CloseBillSQL, billID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (c *pgBillCloser) GetBillStatus(ctx context.Context, billID string) (string, error) {
	var status string
	err := c.db.QueryRowContext(ctx, workflowdef.BillStatusSQL, billID).Scan(&status)
	return status, err
}

// ---------------------------------------------------------------------------
// Stubs
// ---------------------------------------------------------------------------

func sendInvoiceEmailStub(_ context.Context, _ string) error {
	// Stub activity for this assignment.
	return nil
}
