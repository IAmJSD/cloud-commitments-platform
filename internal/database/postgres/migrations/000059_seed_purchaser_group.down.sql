-- Reverse migration: remove the Purchaser group that 000059 seeded (if it
-- seeded one), then drop the system_managed column.
--
-- 000059's INSERT targeted 00000000-0000-5000-8000-000000000005, which
-- 000057 had already given to "Standard Users", so on every database that ran
-- both the INSERT was a silent no-op (issue #942). 000064 then created
-- Purchaser at ...0007 and owns its lifecycle (its own down is a no-op). So
-- the row at ...0005 is only ours to remove when it really is Purchaser.
-- Deleting it unconditionally destroyed Standard Users and stripped it from
-- every member (issue #316, A04-010).
--
-- Not reversed: before issue #316's fix, 000059's admin backfill added ...0005
-- to every Administrators member. Where ...0005 is Standard Users those
-- memberships cannot be told apart from deliberate ones, so they stay. The up
-- now backfills only when ...0005 really is Purchaser.

DO $$
DECLARE
    v_uuid UUID := '00000000-0000-5000-8000-000000000005';
    v_stranded INT;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM groups WHERE id = v_uuid AND name = 'Purchaser') THEN
        RETURN;
    END IF;

    -- Fail loud rather than leave a user with no group (users_min_one_group
    -- would abort anyway, with a less useful message).
    SELECT count(*) INTO v_stranded
      FROM users
     WHERE v_uuid = ANY(group_ids)
       AND cardinality(array_remove(group_ids, v_uuid)) = 0;
    IF v_stranded > 0 THEN
        RAISE EXCEPTION
            'migration 000059 down: % user(s) belong only to Purchaser (%); give them another group before rolling back',
            v_stranded, v_uuid;
    END IF;

    UPDATE users
    SET group_ids = array_remove(group_ids, v_uuid)
    WHERE v_uuid = ANY(group_ids);

    DELETE FROM groups WHERE id = v_uuid AND name = 'Purchaser';
END $$;

-- Drop the system_managed column (rolls back the ALTER TABLE in the up).
ALTER TABLE groups DROP COLUMN IF EXISTS system_managed;
