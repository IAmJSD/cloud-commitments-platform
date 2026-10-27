-- Reverse 000096: detach every user from RI Exchanger, then drop the group.
--
-- Order matters: group_ids is a plain UUID array with no FK, so a dropped
-- group would otherwise leave dangling ids that collectGroupsAndAccounts
-- silently skips, making the rollback look clean while leaving debris.

DO $$
DECLARE
    v_uuid UUID := '00000000-0000-5000-8000-000000000008';
    v_stranded INT;
BEGIN
    -- A user whose only group is RI Exchanger would be left with none, which
    -- users_min_one_group (000057) forbids. Fail loud with an actionable
    -- message instead of the bare CHECK violation (issue #316, A04-010).
    SELECT count(*) INTO v_stranded
      FROM users
     WHERE v_uuid = ANY(COALESCE(group_ids, '{}'))
       AND cardinality(array_remove(group_ids, v_uuid)) = 0;
    IF v_stranded > 0 THEN
        RAISE EXCEPTION
            'migration 000096 down: % user(s) belong only to RI Exchanger (%); give them another group before rolling back',
            v_stranded, v_uuid;
    END IF;

    UPDATE users
    SET group_ids = array_remove(COALESCE(group_ids, '{}'), v_uuid)
    WHERE v_uuid = ANY(COALESCE(group_ids, '{}'));

    -- Only remove the seeded row. A group an operator renamed onto this id
    -- is not ours to drop.
    DELETE FROM groups WHERE id = v_uuid AND name = 'RI Exchanger';
END $$;
