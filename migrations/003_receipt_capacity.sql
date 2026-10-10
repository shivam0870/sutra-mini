-- A maintained counter lets PostgreSQL serialize competing receipts with a
-- conditional UPDATE. This also protects inserts made outside the HTTP API.
ALTER TABLE jobwork_issues ADD COLUMN received_qty bigint NOT NULL DEFAULT 0;
UPDATE jobwork_issues i SET received_qty = COALESCE(
    (SELECT sum(r.qty) FROM jobwork_receipts r WHERE r.issue_id = i.id), 0);
ALTER TABLE jobwork_issues ADD CONSTRAINT issue_received_capacity
    CHECK (received_qty >= 0 AND received_qty <= qty);

CREATE FUNCTION reserve_receipt_capacity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE jobwork_issues
    SET received_qty = received_qty + NEW.qty
    WHERE id = NEW.issue_id AND NEW.qty > 0 AND NEW.qty <= qty - received_qty;
    IF NOT FOUND THEN
        IF NOT EXISTS (SELECT 1 FROM jobwork_issues WHERE id = NEW.issue_id) THEN
            RAISE EXCEPTION 'issue does not exist'
                USING ERRCODE = '23503', CONSTRAINT = 'jobwork_receipts_issue_id_fkey';
        END IF;
        RAISE EXCEPTION 'receipt exceeds outstanding quantity'
            USING ERRCODE = '23514', CONSTRAINT = 'receipt_capacity';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER reserve_receipt_capacity BEFORE INSERT ON jobwork_receipts
FOR EACH ROW EXECUTE FUNCTION reserve_receipt_capacity();

-- Corrections must be explicit accounting reversals, not edits to a delivered
-- receipt. Only the saved response may change inside its creation transaction.
CREATE FUNCTION guard_receipt_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'receipts are immutable' USING ERRCODE = '23514';
    END IF;
    IF ROW(NEW.id, NEW.issue_id, NEW.qty, NEW.received_on, NEW.idempotency_key,
           NEW.request_fingerprint, NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id, OLD.issue_id, OLD.qty, OLD.received_on, OLD.idempotency_key,
           OLD.request_fingerprint, OLD.created_at) THEN
        RAISE EXCEPTION 'receipts are immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER guard_receipt_ledger BEFORE UPDATE OR DELETE ON jobwork_receipts
FOR EACH ROW EXECUTE FUNCTION guard_receipt_ledger();
