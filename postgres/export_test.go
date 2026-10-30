package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// MockPool adapts one scripted connection to the backend's pool seam, so fault
// tests drive the same session code production uses. Every acquire leases the
// same scripted connection. Discard maps to the script's Close expectation;
// release only counts, because a healthy release never closes the physical
// session. Counters are owned by the single test goroutine.
type MockPool struct {
	// Conn scripts every statement, transaction and discard.
	Conn *MockConn
	// OnAcquire, when set, observes the acquisition context and may fail it.
	OnAcquire func(context.Context) error
	// OnQuery, when set, runs before each connection query reaches Conn.
	OnQuery func(string)
	// Released and Discarded count how leased sessions ended.
	Released, Discarded int
	// ConnClosed reports the scripted session as already closed by the driver.
	ConnClosed bool
}

// NewMockBackend constructs a backend over pool with production defaults.
func NewMockBackend(pool *MockPool, options ...Option) (*Backend, error) {
	return newBackend(pool, options...)
}

func (pool *MockPool) acquire(ctx context.Context) (connection, error) {
	if pool.OnAcquire != nil {
		if err := pool.OnAcquire(ctx); err != nil {
			return nil, err
		}
	}

	return &mockConnection{MockConn: pool.Conn, pool: pool}, nil
}

func (pool *MockPool) begin(ctx context.Context, options pgx.TxOptions) (transaction, error) {
	return pool.Conn.begin(ctx, options)
}

type mockConnection struct {
	*MockConn
	pool *MockPool
}

func (connection *mockConnection) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if connection.pool.OnQuery != nil {
		connection.pool.OnQuery(query)
	}

	return connection.MockConn.QueryRow(ctx, query, args...)
}

func (connection *mockConnection) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	if connection.pool.OnQuery != nil {
		connection.pool.OnQuery(query)
	}

	return connection.MockConn.Query(ctx, query, args...)
}

func (connection *mockConnection) release() { connection.pool.Released++ }

func (connection *mockConnection) closed() bool { return connection.pool.ConnClosed }

func (connection *mockConnection) discard(ctx context.Context) error {
	connection.pool.Discarded++

	return connection.Close(ctx)
}

type mockCall int

const (
	mockQuery mockCall = iota
	mockExec
	mockBegin
	mockCommit
	mockRollback
	mockClose
)

var mockCallNames = [...]string{"Query", "Exec", "Begin", "Commit", "Rollback", "Close"}

// MockConn is an ordered script of the statements one physical session
// receives. Transactions share the connection's script, so statements inside
// and outside a transaction keep one order. Unexpected calls return an error and
// consume nothing. Scanning follows pgx strictness: NULL only enters nullable
// destinations and only numeric values convert between Go types.
type MockConn struct {
	mu           sync.Mutex
	expectations []*MockExpectation
}

// NewMockConn returns an empty script.
func NewMockConn() *MockConn { return &MockConn{} }

// MockExpectation is one scripted call and its outcome.
type MockExpectation struct {
	call      mockCall
	pattern   *regexp.Regexp
	args      []any
	checkArgs bool
	options   *pgx.TxOptions
	rows      *MockRows
	result    pgconn.CommandTag
	err       error
	delay     time.Duration
	triggered bool
}

func (conn *MockConn) expect(call mockCall, pattern string) *MockExpectation {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	expectation := &MockExpectation{call: call}
	if pattern != "" {
		expectation.pattern = regexp.MustCompile(pattern)
	}
	conn.expectations = append(conn.expectations, expectation)

	return expectation
}

// ExpectQuery scripts a Query or QueryRow whose SQL matches pattern.
func (conn *MockConn) ExpectQuery(pattern string) *MockExpectation {
	return conn.expect(mockQuery, pattern)
}

// ExpectExec scripts an Exec whose SQL matches pattern.
func (conn *MockConn) ExpectExec(pattern string) *MockExpectation {
	return conn.expect(mockExec, pattern)
}

// ExpectBegin scripts a transaction start with any options.
func (conn *MockConn) ExpectBegin() *MockExpectation { return conn.expect(mockBegin, "") }

// ExpectBeginTx scripts a transaction start with exactly options.
func (conn *MockConn) ExpectBeginTx(options pgx.TxOptions) *MockExpectation {
	expectation := conn.expect(mockBegin, "")
	expectation.options = &options

	return expectation
}

// ExpectCommit scripts a transaction commit.
func (conn *MockConn) ExpectCommit() *MockExpectation { return conn.expect(mockCommit, "") }

// ExpectRollback scripts a transaction rollback.
func (conn *MockConn) ExpectRollback() *MockExpectation { return conn.expect(mockRollback, "") }

// ExpectClose scripts the physical close that discards the session.
func (conn *MockConn) ExpectClose() *MockExpectation { return conn.expect(mockClose, "") }

// ExpectationsWereMet reports the first scripted call that never happened.
func (conn *MockConn) ExpectationsWereMet() error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	for _, expectation := range conn.expectations {
		if !expectation.triggered {
			return fmt.Errorf("unmet expectation: %s", expectation)
		}
	}

	return nil
}

// WithArgs requires exactly these arguments; MockAnyArg matches any value.
func (expectation *MockExpectation) WithArgs(args ...any) *MockExpectation {
	expectation.args, expectation.checkArgs = args, true
	return expectation
}

// WillReturnRows sets the rows a query returns.
func (expectation *MockExpectation) WillReturnRows(rows *MockRows) *MockExpectation {
	expectation.rows = rows
	return expectation
}

// WillReturnResult sets the command tag an Exec returns.
func (expectation *MockExpectation) WillReturnResult(result pgconn.CommandTag) *MockExpectation {
	expectation.result = result
	return expectation
}

// WillReturnError makes the call fail with err.
func (expectation *MockExpectation) WillReturnError(err error) *MockExpectation {
	expectation.err = err
	return expectation
}

// WillDelayFor delays the outcome; context expiry ends the delay with the
// context error, as a context-aware driver does.
func (expectation *MockExpectation) WillDelayFor(delay time.Duration) *MockExpectation {
	expectation.delay = delay
	return expectation
}

func (expectation *MockExpectation) String() string {
	text := mockCallNames[expectation.call]
	if expectation.pattern != nil {
		text += " " + strconv.Quote(expectation.pattern.String())
	}

	return text
}

// MockAnyArg matches any argument value in WithArgs.
func MockAnyArg() any { return anyMockArg{} }

type anyMockArg struct{}

// MockResult returns a command tag reporting rows affected rows.
func MockResult(operation string, rows int64) pgconn.CommandTag {
	return pgconn.NewCommandTag(operation + " " + strconv.FormatInt(rows, 10))
}

func (conn *MockConn) next(ctx context.Context, call mockCall, query string, args []any, options *pgx.TxOptions) (*MockExpectation, error) {
	conn.mu.Lock()
	var expectation *MockExpectation
	for _, candidate := range conn.expectations {
		if !candidate.triggered {
			expectation = candidate
			break
		}
	}
	if expectation == nil {
		conn.mu.Unlock()
		return nil, fmt.Errorf("unexpected %s %q: all expectations were met", mockCallNames[call], query)
	}
	if mismatch := expectation.mismatch(call, query, args, options); mismatch != "" {
		conn.mu.Unlock()
		return nil, fmt.Errorf("unexpected %s %q, next expectation is %s: %s", mockCallNames[call], query, expectation, mismatch)
	}
	expectation.triggered = true
	conn.mu.Unlock()

	if expectation.delay > 0 {
		timer := time.NewTimer(expectation.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return expectation, ctx.Err()
		case <-timer.C:
		}
	}

	return expectation, expectation.err
}

func (expectation *MockExpectation) mismatch(call mockCall, query string, args []any, options *pgx.TxOptions) string {
	switch {
	case expectation.call != call:
		return "call kind differs"
	case expectation.pattern != nil && !expectation.pattern.MatchString(query):
		return "SQL differs"
	case expectation.options != nil && (options == nil || *expectation.options != *options):
		return fmt.Sprintf("transaction options %+v differ", options)
	case expectation.checkArgs && len(expectation.args) != len(args):
		return fmt.Sprintf("argument count %d differs", len(args))
	}
	if expectation.checkArgs {
		for index, want := range expectation.args {
			if _, ok := want.(anyMockArg); !ok && !reflect.DeepEqual(want, args[index]) {
				return fmt.Sprintf("argument %d = %#v, want %#v", index+1, args[index], want)
			}
		}
	}

	return ""
}

// Exec runs one scripted Exec.
func (conn *MockConn) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	expectation, err := conn.next(ctx, mockExec, query, args, nil)
	if err != nil {
		return pgconn.CommandTag{}, err
	}

	return expectation.result, nil
}

// Query runs one scripted Query.
func (conn *MockConn) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	expectation, err := conn.next(ctx, mockQuery, query, args, nil)
	if err != nil {
		return nil, err
	}
	if expectation.rows == nil {
		return nil, errors.New("scripted query has no rows")
	}

	return expectation.rows.start(), nil
}

// QueryRow runs one scripted Query and scans at most its first row.
func (conn *MockConn) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	rows, err := conn.Query(ctx, query, args...)
	return mockRow{rows: rows, err: err}
}

// Commit runs one scripted Commit.
func (conn *MockConn) Commit(ctx context.Context) error {
	_, err := conn.next(ctx, mockCommit, "", nil, nil)
	return err
}

// Rollback runs one scripted Rollback.
func (conn *MockConn) Rollback(ctx context.Context) error {
	_, err := conn.next(ctx, mockRollback, "", nil, nil)
	return err
}

// Close runs one scripted physical Close.
func (conn *MockConn) Close(ctx context.Context) error {
	_, err := conn.next(ctx, mockClose, "", nil, nil)
	return err
}

func (conn *MockConn) begin(ctx context.Context, options pgx.TxOptions) (transaction, error) {
	if _, err := conn.next(ctx, mockBegin, "", nil, &options); err != nil {
		return nil, err
	}

	return &mockTx{MockConn: conn}, nil
}

// mockTx models pgx's finalization state: after Commit or Rollback, even a
// failed one, Rollback returns pgx.ErrTxClosed without reaching the server.
type mockTx struct {
	*MockConn
	finished bool
}

func (tx *mockTx) Commit(ctx context.Context) error {
	if tx.finished {
		return pgx.ErrTxClosed
	}
	tx.finished = true

	return tx.MockConn.Commit(ctx)
}

func (tx *mockTx) Rollback(ctx context.Context) error {
	if tx.finished {
		return pgx.ErrTxClosed
	}
	tx.finished = true

	return tx.MockConn.Rollback(ctx)
}

type mockRow struct {
	rows pgx.Rows
	err  error
}

func (row mockRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	defer row.rows.Close()
	if !row.rows.Next() {
		if err := row.rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}

	return row.rows.Scan(dest...)
}

// MockRows is scripted query output. One value may be returned by one query.
type MockRows struct {
	columns   []string
	values    [][]any
	rowErrors map[int]error
}

// NewMockRows returns empty rows with columns.
func NewMockRows(columns []string) *MockRows {
	return &MockRows{columns: columns, rowErrors: make(map[int]error)}
}

// AddRow appends one row of column values.
func (rows *MockRows) AddRow(values ...any) *MockRows {
	rows.values = append(rows.values, values)
	return rows
}

// RowError makes advancing to row index fail with err, as a broken stream
// does: Next returns false and Err reports err.
func (rows *MockRows) RowError(index int, err error) *MockRows {
	rows.rowErrors[index] = err
	return rows
}

func (rows *MockRows) start() *mockRowsCursor {
	return &mockRowsCursor{rows: rows, position: -1}
}

type mockRowsCursor struct {
	rows     *MockRows
	position int
	err      error
	closed   bool
}

func (cursor *mockRowsCursor) Close() { cursor.closed = true }

func (cursor *mockRowsCursor) Err() error { return cursor.err }

func (cursor *mockRowsCursor) CommandTag() pgconn.CommandTag {
	return MockResult("SELECT", int64(len(cursor.rows.values)))
}

func (*mockRowsCursor) FieldDescriptions() []pgconn.FieldDescription { return nil }

func (cursor *mockRowsCursor) Next() bool {
	if cursor.closed || cursor.err != nil {
		return false
	}
	cursor.position++
	if err, ok := cursor.rows.rowErrors[cursor.position]; ok {
		cursor.err = err
		cursor.closed = true
		return false
	}
	if cursor.position >= len(cursor.rows.values) {
		cursor.closed = true
		return false
	}

	return true
}

func (cursor *mockRowsCursor) Scan(dest ...any) error {
	if cursor.closed || cursor.position < 0 || cursor.position >= len(cursor.rows.values) {
		return errors.New("scan without current row")
	}
	values := cursor.rows.values[cursor.position]
	if len(dest) != len(values) || len(values) != len(cursor.rows.columns) {
		return fmt.Errorf("scan of %d values into %d destinations", len(values), len(dest))
	}
	for index, value := range values {
		if err := assignMockValue(dest[index], value); err != nil {
			return fmt.Errorf("column %s: %w", cursor.rows.columns[index], err)
		}
	}

	return nil
}

func (cursor *mockRowsCursor) Values() ([]any, error) {
	return cursor.rows.values[cursor.position], nil
}

func (*mockRowsCursor) RawValues() [][]byte { return nil }

func (*mockRowsCursor) Conn() *pgx.Conn { return nil }

func (*mockRowsCursor) TypeMap() *pgtype.Map { return nil }

func assignMockValue(dest, value any) error {
	if scanner, ok := dest.(interface{ Scan(any) error }); ok {
		return scanner.Scan(value)
	}
	target := reflect.ValueOf(dest)
	if target.Kind() != reflect.Pointer || target.IsNil() {
		return fmt.Errorf("destination %T is not a pointer", dest)
	}
	element := target.Elem()
	if value == nil {
		switch element.Kind() {
		case reflect.Slice, reflect.Pointer, reflect.Interface, reflect.Map:
			element.Set(reflect.Zero(element.Type()))
			return nil
		default:
			return fmt.Errorf("cannot scan NULL into %s", element.Type())
		}
	}
	source := reflect.ValueOf(value)
	if source.Type().AssignableTo(element.Type()) {
		element.Set(source)
		return nil
	}
	if numericKind(source.Kind()) && numericKind(element.Kind()) {
		element.Set(source.Convert(element.Type()))
		return nil
	}

	return fmt.Errorf("cannot scan %T into %s", value, element.Type())
}

func numericKind(kind reflect.Kind) bool {
	return kind >= reflect.Int && kind <= reflect.Uint64
}
