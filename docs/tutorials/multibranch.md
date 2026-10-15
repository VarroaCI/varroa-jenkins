# Work with multibranch jobs

A multibranch job builds each branch and each pull request of a repository. A cell
discovers the heads and lists them on the job page.

## Read the inventory

1. Open the team in the dashboard. Click the name of a multibranch job.
2. The page shows two cards: **Branches** and **Pull requests**.
3. Each row names a head. Click the name to open the head's job page.
4. The **Latest run** column shows the result of the newest build of the head. Click the
   result to open that build.
5. A head marked **orphaned** no longer exists in the repository, or Jenkins no longer
   discovers it as buildable.

The Run control does not appear on a multibranch job. Run a head from its own job page.

## Know how fresh the list is

- The cell exports the list. The list can be up to ten minutes old. The page shows the
  time of the last export.
- A cell that is down or hibernated keeps its last list. The page shows that list.
- A job with no export yet shows empty cards and the note "The cell has not exported this
  job yet." This is normal for a new job.
- The list is capped. A note says so when the cell omits heads.

## Scan the branches

Select **Scan now** on the job page to start a scan. The button appears only when you may
write the job.

- The scan runs on the cell that holds the job's home slot.
- The page checks for the result every five seconds. The checks stop when the result
  arrives, or after ten minutes.
- A completed scan shows the counts of new, changed, and orphaned heads. Select a count to
  see the names. The list shows only the heads that you may read, including a head that the
  scan deleted.
- A scan that does not complete shows its state and reason:
  - `failed`: Jenkins did not finish the index.
  - `timeout`: the scan ran for more than 30 minutes.
  - `refused`: the cell did not run the scan. You need the build permission on the job.
- For 15 minutes after a scan starts, the cell exports the list at its shortest interval.
  The result then reaches the list within about one minute.

The MCP tool `scan_multibranch` starts the same scan. Read its result with
`list_job_branches`, in the `lastScan` value.

## Read the inventory from the command line or an agent

The MCP tool `list_job_branches` returns the same lists as the page. It takes the `team`
and `job` arguments. The HTTP route is `GET /api/v1/pipelines/{team}/jobs/{job}/branches`.
The scan route is `POST /api/v1/pipelines/{team}/jobs/{job}/scan`.

A head you may not read does not appear in the response.

## See also

- [Read and run pipelines](pipelines.md)
- [Build records](../operations/build-records.md)
