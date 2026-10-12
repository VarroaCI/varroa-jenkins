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

## Read the inventory from the command line or an agent

The MCP tool `list_job_branches` returns the same lists as the page. It takes the `team`
and `job` arguments. The HTTP route is `GET /api/v1/pipelines/{team}/jobs/{job}/branches`.

A head you may not read does not appear in the response.

## See also

- [Read and run pipelines](pipelines.md)
- [Build records](../operations/build-records.md)
