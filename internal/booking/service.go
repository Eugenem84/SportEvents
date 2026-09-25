package booking

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Notifier is called after a booking is promoted from waitlist to
// confirmed. The domain rule is: a promoted booking — especially a guest
// without an identity — must not be silently moved to confirmed. The
// implementation decides whom to notify: the participant (via identity),
// the booked_by user, or the chat.
type Notifier interface {
	NotifyPromotion(ctx context.Context, promoted Booking) error
}

type Service struct {
	pool   *pgxpool.Pool
	notify Notifier
}

// NewService builds a Booking Service. A notifier may be passed to satisfy
// the promotion-notification invariant. Without one, promotions are still
// returned via CancelResult.Promoted for the caller to act on.
func NewService(pool *pgxpool.Pool, notifiers ...Notifier) *Service {
	s := &Service{pool: pool}
	if len(notifiers) > 0 {
		s.notify = notifiers[0]
	}
	return s
}

// Create records a new participant for an event. It locks the event row so
// concurrent bookings are serialized, then decides confirmed vs waitlist
// based on the current confirmed count against capacity. If the user
// already has an active booking (confirmed or waitlist) for this event,
// ErrAlreadyBooked is returned.
func (s *Service) Create(ctx context.Context, in CreateInput) (Booking, error) {
	if err := validateCreate(in); err != nil {
		return Booking{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Booking{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var capacity int
	err = tx.QueryRow(ctx,
		`SELECT capacity FROM events WHERE id = $1 FOR UPDATE`,
		in.EventID,
	).Scan(&capacity)
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, ErrEventNotFound
	}
	if err != nil {
		return Booking{}, fmt.Errorf("lock event: %w", err)
	}

	if in.UserID != nil {
		var exists bool
		err = tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM bookings
				WHERE event_id = $1 AND user_id = $2 AND status IN ('confirmed', 'waitlist')
			)`,
			in.EventID, *in.UserID,
		).Scan(&exists)
		if err != nil {
			return Booking{}, fmt.Errorf("check existing booking: %w", err)
		}
		if exists {
			return Booking{}, ErrAlreadyBooked
		}
	}

	var confirmed int
	err = tx.QueryRow(ctx,
		`SELECT count(*)::int FROM bookings WHERE event_id = $1 AND status = 'confirmed'`,
		in.EventID,
	).Scan(&confirmed)
	if err != nil {
		return Booking{}, fmt.Errorf("count confirmed: %w", err)
	}

	status := StatusConfirmed
	if confirmed >= capacity {
		status = StatusWaitlist
	}

	// Подтверждённая запись получает наименьший свободный номер места. Строку
	// события мы уже держим под FOR UPDATE, поэтому номер не отберут параллельно.
	var seat *int
	if status == StatusConfirmed {
		seat, err = freeSeat(ctx, tx, in.EventID, capacity)
		if err != nil {
			return Booking{}, err
		}
		if seat == nil {
			// Мест нет, хотя confirmed < capacity: данные разошлись. Отправляем
			// в резерв, а не создаём вторую запись на то же место.
			status = StatusWaitlist
		}
	}

	var phone *string
	if in.Phone != "" {
		phone = &in.Phone
	}

	const q = `
		INSERT INTO bookings (event_id, player_name, phone, user_id, booked_by_user_id, seat_no, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, event_id, player_name, phone, user_id, booked_by_user_id, seat_no, status, created_at`

	var b Booking
	var statusStr string
	err = tx.QueryRow(ctx, q,
		in.EventID, in.PlayerName, phone, in.UserID, in.BookedByUserID, seat, string(status),
	).Scan(&b.ID, &b.EventID, &b.PlayerName, &b.Phone, &b.UserID, &b.BookedByUserID, &b.SeatNo, &statusStr, &b.CreatedAt)
	if isUniqueViolation(err) {
		return Booking{}, ErrAlreadyBooked
	}
	if isForeignKey(err) {
		return Booking{}, ErrUserNotFound
	}
	if err != nil {
		return Booking{}, fmt.Errorf("create booking: %w", err)
	}
	b.Status = Status(statusStr)

	if err := tx.Commit(ctx); err != nil {
		return Booking{}, fmt.Errorf("commit: %w", err)
	}
	return b, nil
}

// Cancel marks a booking as cancelled and, if it was confirmed, promotes
// the first waitlist booking (FIFO by created_at, id) to confirmed. The
// event row is locked for the duration of the transaction so promotion is
// consistent with concurrent Create/Cancel calls.
//
// After the commit, when a promotion happened, the service calls
// NotifyPromotion so the "no silent promotion" invariant holds even for
// guests. If the notifier fails, the cancellation is already committed: the
// error is returned alongside the result, and a retry will surface
// ErrNotFound.
func (s *Service) Cancel(ctx context.Context, eventID, bookingID int64) (CancelResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CancelResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var capacity int
	err = tx.QueryRow(ctx, `SELECT capacity FROM events WHERE id = $1 FOR UPDATE`, eventID).Scan(&capacity)
	if errors.Is(err, pgx.ErrNoRows) {
		return CancelResult{}, ErrEventNotFound
	}
	if err != nil {
		return CancelResult{}, fmt.Errorf("lock event: %w", err)
	}

	target, err := scanBooking(tx.QueryRow(ctx, `
		SELECT id, event_id, player_name, phone, user_id, booked_by_user_id, seat_no, status, created_at
		FROM bookings
		WHERE id = $1 AND event_id = $2
		FOR UPDATE`,
		bookingID, eventID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return CancelResult{}, ErrNotFound
	}
	if err != nil {
		return CancelResult{}, fmt.Errorf("lock booking: %w", err)
	}
	if target.Status == StatusCancelled {
		return CancelResult{}, ErrNotActive
	}

	wasConfirmed := target.Status == StatusConfirmed

	_, err = tx.Exec(ctx, `UPDATE bookings SET status = 'cancelled' WHERE id = $1`, target.ID)
	if err != nil {
		return CancelResult{}, fmt.Errorf("cancel booking: %w", err)
	}
	target.Status = StatusCancelled

	result := CancelResult{Cancelled: target}

	if wasConfirmed {
		promoted, err := scanBooking(tx.QueryRow(ctx, `
			SELECT id, event_id, player_name, phone, user_id, booked_by_user_id, seat_no, status, created_at
			FROM bookings
			WHERE event_id = $1 AND status = 'waitlist'
			ORDER BY created_at ASC, id ASC
			LIMIT 1
			FOR UPDATE`,
			eventID,
		))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return CancelResult{}, fmt.Errorf("find waitlist: %w", err)
		}
		if err == nil {
			// Освободившееся место отдаём именно с этим номером, чтобы нумерация
			// в анонсе не сдвигалась. Если у отменённой записи номера не было
			// (данные до миграции), берём наименьший свободный.
			seat := target.SeatNo
			if seat == nil {
				seat, err = freeSeat(ctx, tx, eventID, capacity)
				if err != nil {
					return CancelResult{}, err
				}
			}
			_, err = tx.Exec(ctx,
				`UPDATE bookings SET status = 'confirmed', seat_no = $2 WHERE id = $1`,
				promoted.ID, seat,
			)
			if err != nil {
				return CancelResult{}, fmt.Errorf("promote booking: %w", err)
			}
			promoted.Status = StatusConfirmed
			promoted.SeatNo = seat
			result.Promoted = &promoted
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return CancelResult{}, fmt.Errorf("commit: %w", err)
	}

	if result.Promoted != nil && s.notify != nil {
		if err := s.notify.NotifyPromotion(ctx, *result.Promoted); err != nil {
			return result, fmt.Errorf("notify promotion: %w", err)
		}
	}
	return result, nil
}

// Get returns a booking scoped to an event, mirroring how the Event
// service scopes an event to a chat.
func (s *Service) Get(ctx context.Context, eventID, bookingID int64) (Booking, error) {
	b, err := scanBooking(s.pool.QueryRow(ctx, `
		SELECT id, event_id, player_name, phone, user_id, booked_by_user_id, seat_no, status, created_at
		FROM bookings
		WHERE id = $1 AND event_id = $2`,
		bookingID, eventID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, ErrNotFound
	}
	if err != nil {
		return Booking{}, fmt.Errorf("get booking: %w", err)
	}
	return b, nil
}

// ListByEvent returns bookings for an event. If statuses is empty, all
// statuses are returned. Ordering is created_at, id ascending, which is
// also the FIFO order used for the waitlist.
func (s *Service) ListByEvent(ctx context.Context, eventID int64, statuses ...Status) ([]Booking, error) {
	q := `
		SELECT id, event_id, player_name, phone, user_id, booked_by_user_id, seat_no, status, created_at
		FROM bookings
		WHERE event_id = $1`
	args := []any{eventID}
	if len(statuses) > 0 {
		strs := make([]string, len(statuses))
		for i, st := range statuses {
			strs[i] = string(st)
		}
		q += " AND status = ANY($2)"
		args = append(args, strs)
	}
	q += " ORDER BY created_at ASC, id ASC"

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list bookings: %w", err)
	}
	defer rows.Close()

	var out []Booking
	for rows.Next() {
		b, err := scanBooking(rows)
		if err != nil {
			return nil, fmt.Errorf("scan booking: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list bookings: %w", err)
	}
	if out == nil {
		out = []Booking{}
	}
	return out, nil
}

// ListActiveByUser returns the user's active bookings (confirmed and
// waitlist) for events starting at or after from, ordered by event time.
// Guests (user_id IS NULL) never appear here: they have no identity to ask
// for "my bookings".
func (s *Service) ListActiveByUser(ctx context.Context, userID int64, from time.Time) ([]BookingWithEvent, error) {
	const q = `
		SELECT b.id, b.event_id, b.player_name, b.phone, b.user_id, b.booked_by_user_id,
		       b.seat_no, b.status, b.created_at,
		       e.title, e.starts_at, e.location, e.capacity
		FROM bookings b
		JOIN events e ON e.id = b.event_id
		WHERE b.user_id = $1
		  AND b.status IN ('confirmed', 'waitlist')
		  AND e.starts_at >= $2
		ORDER BY e.starts_at ASC, b.id ASC`

	rows, err := s.pool.Query(ctx, q, userID, from.UTC())
	if err != nil {
		return nil, fmt.Errorf("list user bookings: %w", err)
	}
	defer rows.Close()

	var out []BookingWithEvent
	for rows.Next() {
		var b BookingWithEvent
		var statusStr string
		if err := rows.Scan(
			&b.ID, &b.EventID, &b.PlayerName, &b.Phone, &b.UserID, &b.BookedByUserID,
			&b.SeatNo, &statusStr, &b.CreatedAt,
			&b.EventTitle, &b.EventStartsAt, &b.EventLocation, &b.EventCapacity,
		); err != nil {
			return nil, fmt.Errorf("scan user booking: %w", err)
		}
		b.Status = Status(statusStr)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list user bookings: %w", err)
	}
	if out == nil {
		out = []BookingWithEvent{}
	}
	return out, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanBooking(row rowScanner) (Booking, error) {
	var b Booking
	var statusStr string
	err := row.Scan(&b.ID, &b.EventID, &b.PlayerName, &b.Phone, &b.UserID, &b.BookedByUserID,
		&b.SeatNo, &statusStr, &b.CreatedAt)
	if err != nil {
		return Booking{}, err
	}
	b.Status = Status(statusStr)
	return b, nil
}

// freeSeat returns the lowest free seat of an event, or nil when every seat is
// taken. The caller holds the event row lock, so nothing can take the number
// in between.
func freeSeat(ctx context.Context, tx pgx.Tx, eventID int64, capacity int) (*int, error) {
	var seat int
	err := tx.QueryRow(ctx, `
		SELECT seat
		FROM generate_series(1, $2::int) AS seat
		WHERE NOT EXISTS (
			SELECT 1
			FROM bookings b
			WHERE b.event_id = $1 AND b.status = 'confirmed' AND b.seat_no = seat
		)
		ORDER BY seat
		LIMIT 1`,
		eventID, capacity,
	).Scan(&seat)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("free seat: %w", err)
	}
	return &seat, nil
}

func validateCreate(in CreateInput) error {
	if in.EventID == 0 {
		return ErrInvalid
	}
	if strings.TrimSpace(in.PlayerName) == "" {
		return ErrInvalid
	}
	if in.BookedByUserID == 0 {
		return ErrInvalid
	}
	return nil
}

func isForeignKey(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
