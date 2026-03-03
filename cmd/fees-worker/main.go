package main

import (
	"database/sql"
	"log"
	"os"
	"time"

	"pave-assignment/billing/workflowdef"

	_ "github.com/jackc/pgx/v5/stdlib"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// queryTimeout is the maximum duration for a single DB query or transaction.
const queryTimeout = 10 * time.Second

func main() {
	hostPort := os.Getenv("TEMPORAL_ADDRESS")
	if hostPort == "" {
		hostPort = "127.0.0.1:7233"
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

	// Connection pool tuning — prevent unbounded connection growth.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

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

	persister := &pgLineItemPersister{db: db}

	activities := workflowdef.NewActivities(
		closeBillFunc(db),
		sendInvoiceEmailStub,
		persister.PersistLineItem,
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
	w.RegisterActivityWithOptions(activities.PersistLineItem, activity.RegisterOptions{
		Name: workflowdef.PersistLineItemActivityName,
	})

	log.Println("starting billing worker")
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("unable to run worker: %v", err)
	}
}
