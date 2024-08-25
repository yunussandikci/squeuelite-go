package engines

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	"github.com/yunussandikci/dbqueue-go/dbqueue/types"
	"strconv"
	"time"
)

type sqliteEngine struct {
	db *sql.DB
}
type sqliteQueue struct {
	db    *sql.DB
	table string
}

func NewSQLiteEngine(ctx context.Context, conn string) (types.Engine, error) {
	db, newErr := sql.Open("sqlite3", conn)
	if newErr != nil {
		return nil, newErr
	}
	return &sqliteEngine{
		db: db,
	}, nil
}

func (p *sqliteEngine) OpenQueue(name string) (types.Queue, error) {
	var exists int
	query := `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?;`
	if queryErr := p.db.QueryRowContext(context.Background(), query, name).Scan(&exists); queryErr != nil {
		return nil, queryErr
	}

	if exists == 0 {
		return nil, types.ErrQueueNotFound
	}

	return &sqliteQueue{
		db:    p.db,
		table: name,
	}, nil
}

func (p *sqliteEngine) CreateQueue(name string) (types.Queue, error) {
	query := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				deduplication_id TEXT NOT NULL UNIQUE,
				payload BLOB,
				priority INTEGER NOT NULL DEFAULT 0,
				retrieval INTEGER NOT NULL DEFAULT 0,
				visible_after INTEGER NOT NULL DEFAULT (strftime('%%s', 'now')),
				created_at INTEGER NOT NULL DEFAULT (strftime('%%s', 'now')));`, name)
	_, execErr := p.db.ExecContext(context.Background(), query)
	return &sqliteQueue{
		db:    p.db,
		table: name,
	}, errors.Join(execErr)
}

func (p *sqliteEngine) DeleteQueue(name string) error {
	query := fmt.Sprintf("DROP TABLE IF EXISTS %s;", name)
	_, execErr := p.db.ExecContext(context.Background(), query)
	return execErr
}

func (p *sqliteEngine) PurgeQueue(name string) error {
	query := fmt.Sprintf("DELETE FROM %s;", name)
	_, execErr := p.db.ExecContext(context.Background(), query)
	return execErr
}

func (p *sqliteQueue) ReceiveMessage(fun func(message types.ReceivedMessage), options types.ReceiveMessageOptions) error {
	opts := options.Defaults()
	limit := strconv.Itoa(*opts.MaxNumberOfMessages)
	if *opts.MaxNumberOfMessages == 0 {
		limit = "-1"
	}

	for {
		query := fmt.Sprintf(`UPDATE %s 
		SET retrieval = retrieval + 1, visible_after = %d
		WHERE id IN (
			SELECT id FROM %s 
			WHERE visible_after < %d
			ORDER BY priority DESC, id ASC 
			LIMIT %s
		)
		RETURNING id, deduplication_id, payload, priority, visible_after, retrieval, created_at;`,
			p.table, time.Now().Add(*opts.VisibilityTimeout).Unix(), p.table, time.Now().Unix(), limit)

		rows, err := p.db.QueryContext(context.Background(), query)
		if err != nil {
			return err
		}

		var messages []*types.ReceivedMessage
		for rows.Next() {
			var newMessage types.ReceivedMessage
			if scanErr := rows.Scan(&newMessage.ID, &newMessage.DeduplicationID, &newMessage.Payload,
				&newMessage.Priority, &newMessage.VisibleAfter, &newMessage.Retrieval,
				&newMessage.CreatedAt); scanErr != nil {
				return scanErr
			}

			messages = append(messages, &newMessage)
		}

		if rowsErr := rows.Err(); rowsErr != nil {
			return rowsErr
		}

		if closeErr := rows.Close(); closeErr != nil {
			return closeErr
		}

		for _, message := range messages {
			fun(*message)
		}

		if len(messages) == 0 {
			time.Sleep(*opts.WaitTime)
		}
	}
}

func (p *sqliteQueue) SendMessage(message *types.Message) error {
	return p.SendMessageBatch([]*types.Message{message})
}

func (p *sqliteQueue) SendMessageBatch(messages []*types.Message) error {
	transaction, beginTransactionErr := p.db.BeginTx(context.Background(), nil)
	if beginTransactionErr != nil {
		return beginTransactionErr
	}

	query := fmt.Sprintf(`INSERT OR IGNORE INTO %s 
		(deduplication_id, payload, priority, visible_after) 
		VALUES (?, ?, ?, COALESCE(?, strftime('%%s','now')));`, p.table)

	statement, prepareErr := transaction.Prepare(query)
	if prepareErr != nil {
		return errors.Join(prepareErr, transaction.Rollback())
	}

	for _, message := range messages {
		var deduplicationID string
		if message.DeduplicationID == nil {
			deduplicationID = uuid.NewString()
		} else {
			deduplicationID = *message.DeduplicationID
		}

		_, execErr := statement.Exec(deduplicationID, message.Payload, message.Priority, nil)
		if execErr != nil {
			return errors.Join(execErr, transaction.Rollback())
		}
	}

	return errors.Join(transaction.Commit(), statement.Close())
}

func (p *sqliteQueue) DeleteMessage(id uint) error {
	return p.DeleteMessageBatch([]uint{id})
}

func (p *sqliteQueue) DeleteMessageBatch(ids []uint) error {
	query := fmt.Sprintf(`DELETE FROM %s WHERE id = ?;`, p.table)
	stmt, err := p.db.PrepareContext(context.Background(), query)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, id := range ids {
		_, err = stmt.Exec(id)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *sqliteQueue) ChangeMessageVisibility(id uint, visibilityTimeout time.Duration) error {
	return p.ChangeMessageVisibilityBatch([]uint{id}, visibilityTimeout)
}

func (p *sqliteQueue) ChangeMessageVisibilityBatch(ids []uint, visibilityTimeout time.Duration) error {
	query := fmt.Sprintf(`UPDATE %s SET visible_after = ? WHERE id = ?;`, p.table)
	stmt, err := p.db.PrepareContext(context.Background(), query)
	if err != nil {
		return err
	}
	defer stmt.Close()

	visibleAfter := time.Now().Add(visibilityTimeout).Unix()
	for _, id := range ids {
		_, err = stmt.Exec(visibleAfter, id)
		if err != nil {
			return err
		}
	}

	return nil
}