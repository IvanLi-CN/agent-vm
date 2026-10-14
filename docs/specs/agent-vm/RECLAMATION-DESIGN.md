# Reclamation design

Release normally leaves a stopped disposable cache. A cache becomes eligible only after its effective lease has ended, native stop is confirmed and every owned access mapping has been removed. Eligibility does not imply immediate deletion.

Manual `admin gc` is dry-run by default and selects all eligible stopped caches without an idle-age floor. Applying a preview reacquires object locks and rechecks identity, generation, lease state and native cleanup before deleting. A resumed cache, valid lease, transition, resize target or unknown resource is protected.

The age timer independently selects caches idle for at least the configured cache interval. Capacity admission may select eligible caches oldest first until the required local space is available. Neither path stops a valid lease or bypasses the configured storage boundary.
