<script setup lang="ts">
// Reconciliation: whether the numbers agree.
//
// Two lists, whole-plane. A **run** is one pass — a window it scanned, how
// many buckets it read, and how many findings it opened, re-confirmed or left
// alone. A **finding** is one divergence a pass wrote, with the figures it
// compared in `observed`.
//
// **A run is not an account's and a finding is not a bucket's.** Both
// operations take no account and no account-scoped filter, and this screen
// renders them as system-wide on purpose: a reader who filtered findings by an
// account would be reading a subset and reading it as a report about their own
// money, which is the question the plane split exists to keep separate. An
// open finding about another account's bucket is visible here because the
// system has a divergence to report, not because the account behind it is the
// reader's.
//
// **The evidence is the server's bytes.** `observed` is whatever this plane's
// own check wrote, and its shape is not the contract's to fix — so it is shown
// as the JSON it is, formatted, and never parsed into a table of columns this
// screen invented. A console that turned the evidence into its own summary is
// making claims about the figures a second time, and a claim it cannot make.
//
// **Severity is written once and never moved** (the contract says so on the
// field), so the two filters are independent: an operator narrowing to
// `critical` is not thereby saying it is open, and a finding that is resolved
// and critical is one they can still find.
import { Card, PageHeader, SegmentedControl, Stack } from "@ecoma-io/loom";
import { computed } from "vue";

import DataTable from "@/components/DataTable.vue";
import { fetchFindings, fetchReconciliationRuns } from "@/lib/api";
import { usePagedList } from "@/lib/paged-list";
import type { DataTableColumn } from "@/components/data-table";
import FailureView from "@/modules/failure/FailureView.vue";
import InstantCell from "@/modules/status/InstantCell.vue";
import StatusBadge from "@/modules/status/StatusBadge.vue";
import {
  FINDING_SEVERITY_PRESENTATION,
  FINDING_STATUS_PRESENTATION,
  RECONCILIATION_RUN_STATUS_PRESENTATION,
} from "@/modules/status/presentation";
import type {
  Finding,
  FindingPage,
  ListFindingsData,
  ListReconciliationRunsData,
  ReconciliationRun,
  ReconciliationRunPage,
} from "@ecoma-io/llm-gateway-console-api-client";

const runs = usePagedList<ReconciliationRunPage, ListReconciliationRunsData["query"]>({
  read: (query) => fetchReconciliationRuns({ query }),
  // First list on the route, so it keeps the contract's own name for the
  // cursor; the second has to disagree. See the findings list for why.
  shape: { filters: [], cursor: "after" },
  vocabulary: {},
});

const FINDING_STATUSES: readonly Finding["status"][] = ["open", "acknowledged", "resolved"];
const FINDING_SEVERITIES: readonly Finding["severity"][] = ["info", "warning", "critical"];

const findings = usePagedList<FindingPage, ListFindingsData["query"]>({
  read: (query) => fetchFindings({ query }),
  // Two filters, both declared, both in the pager's links — so a page change
  // keeps them, and a filter change drops the cursor because the cursor
  // carries a fingerprint of the filters that minted it.
  //
  // And a NAMED cursor key, because this is the second list on a route and
  // `cursor` defaults to `after` for every list that does not name its own. The
  // two lists would then be reading and writing one key, and the failure is not
  // cosmetic: page the findings and the runs list re-reads with a findings
  // cursor against `GET /reconciliation/runs`, the server refuses it because
  // the cursor names a different collection, and the refusal — arriving as
  // `invalid_request` — is treated as a lost cursor, so `paged-list.ts` fires
  // `restartWithoutCursor(carryOver())` and drops `after` from the route
  // altogether. Both tables return to page one and the findings position the
  // reader had just chosen is gone with it.
  shape: { filters: ["status", "severity"], cursor: "findings_after" },
  vocabulary: { status: FINDING_STATUSES, severity: FINDING_SEVERITIES },
});

const statusOptions = [
  { value: "", label: "All" },
  ...FINDING_STATUSES.map((value) => ({ value, label: FINDING_STATUS_PRESENTATION[value].label })),
];

const severityOptions = [
  { value: "", label: "All" },
  ...FINDING_SEVERITIES.map((value) => ({
    value,
    label: FINDING_SEVERITY_PRESENTATION[value].label,
  })),
];

const statusValue = computed({
  get: () => findings.filter("status") ?? "",
  set: (value: string) => findings.setFilter("status", value),
});

const severityValue = computed({
  get: () => findings.filter("severity") ?? "",
  set: (value: string) => findings.setFilter("severity", value),
});

const runColumns: readonly DataTableColumn[] = [
  { key: "id", label: "Run", align: "right" },
  { key: "status", label: "Status" },
  { key: "window", label: "Window" },
  { key: "buckets_scanned", label: "Buckets", align: "right" },
  { key: "findings_opened", label: "Opened", align: "right" },
  { key: "findings_unchanged", label: "Unchanged", align: "right" },
  { key: "finished_at", label: "Finished" },
];

const findingColumns: readonly DataTableColumn[] = [
  { key: "id", label: "Finding", class: "font-mono text-xs" },
  { key: "check_kind", label: "Check" },
  { key: "subject", label: "Subject" },
  { key: "severity", label: "Severity" },
  { key: "status", label: "Status" },
  { key: "detected_at", label: "Detected" },
  { key: "last_seen_at", label: "Last seen" },
];

/**
 * The window, rendered as both halves of a half-open interval.
 *
 * The contract states the high-water mark is exclusive and is what the NEXT
 * pass opens from, and says a window rendered as an inclusive pair cannot be
 * told from one that overlapped the last. So the column says which end is
 * which; a reader comparing two adjacent runs is comparing `to` of one with
 * `from` of the next, and a rendering that made them look like the same
 * instant would be the one place on this screen a reader could be misled.
 */
function windowText(run: ReconciliationRun): string {
  return `${run.window_from} → ${run.window_to} (to exclusive)`;
}

/**
 * The subject, as the check wrote it: a kind and an id.
 *
 * A finding about another account's bucket is rendered here without its
 * account, because the operation returns none — the authorization predicate is
 * the plane's, and a console that reached for one would be reaching past the
 * operation it was given.
 */
function subjectText(finding: Finding): string {
  return `${finding.subject_kind} ${finding.subject_id}`;
}

/**
 * The runs table's state word: three states, not the two
 * `loading ? "loading" : "empty"` gives.
 *
 * `usePagedList` fires its read from `onMounted`, after the first render, so on
 * that first paint `loading` is false, `data` is `undefined` and `rows` is
 * empty. Reading those as "empty" makes the table claim the gateway has run no
 * reconciliation — a fact about the system's health, asserted by a page that
 * has not yet asked. `data === undefined` is the probe that distinguishes
 * "nothing has come back" from "nothing was there", which is the distinction
 * `resource.ts` exists to keep.
 */
function runsTableState(): "ready" | "loading" | "empty" {
  if (runs.loading.value) return "loading";
  if (runs.data.value === undefined) return "loading";
  return runs.rows.value.length === 0 ? "empty" : "ready";
}

/**
 * The findings table's state word, and the same three states.
 *
 * The boundary this function is careful about is `DataTable.vue`'s `note`
 * prop, which is rendered UNCONDITIONALLY rather than under the `state` gate
 * that guards `emptyMessage`. So returning "loading" here suppresses the empty
 * sentence but not the note, and a `note` that reads "Findings are scoped to
 * the whole system" will be on screen before the system has answered. That is
 * a boundary in a component outside this file; the state word is still the
 * honest one, and the note's ungated rendering is reported rather than worked
 * around here, because changing it would mean editing a component this screen
 * does not own.
 */
function findingsTableState(): "ready" | "loading" | "empty" {
  if (findings.loading.value) return "loading";
  if (findings.data.value === undefined) return "loading";
  return findings.rows.value.length === 0 ? "empty" : "ready";
}
</script>

<template>
  <Stack gap="lg">
    <PageHeader
      title="Reconciliation"
      description="Whole-system passes, and the divergences they found."
    />

    <section aria-labelledby="reconciliation-runs" class="flex flex-col gap-3">
      <h2 id="reconciliation-runs" class="text-lg font-semibold text-foreground">Runs</h2>

      <FailureView
        v-if="runs.failure.value"
        :failure="runs.failure.value"
        :on-retry="() => runs.run()"
        :retrying="runs.loading.value"
      />

      <DataTable
        v-else
        caption="Reconciliation runs across the system"
        layer="page"
        :columns="runColumns"
        :rows="runs.rows.value"
        :state="runsTableState()"
        :pages="runs.pages.value"
        empty-message="No pass has run yet."
      >
        <template #status="{ row }: { row: ReconciliationRun }">
          <StatusBadge :status="RECONCILIATION_RUN_STATUS_PRESENTATION[row.status]" />
        </template>
        <template #window="{ row }: { row: ReconciliationRun }">
          <span class="font-mono text-xs">{{ windowText(row) }}</span>
        </template>
        <template #finished_at="{ row }: { row: ReconciliationRun }">
          <InstantCell :value="row.finished_at" />
        </template>
      </DataTable>

      <p class="text-xs text-muted-foreground">
        "Unchanged" is a figure for a healthy pass, not a disappointment: it counts divergences a
        re-run saw again, which is a confirmation rather than a new finding.
      </p>
    </section>

    <section aria-labelledby="reconciliation-findings" class="flex flex-col gap-3">
      <h2 id="reconciliation-findings" class="text-lg font-semibold text-foreground">Findings</h2>

      <div class="flex flex-wrap items-center gap-4">
        <SegmentedControl
          v-model="statusValue"
          :options="statusOptions"
          aria-label="Filter findings by status"
        />
        <SegmentedControl
          v-model="severityValue"
          :options="severityOptions"
          aria-label="Filter findings by severity"
        />
      </div>

      <FailureView
        v-if="findings.failure.value"
        :failure="findings.failure.value"
        :on-retry="() => findings.run()"
        :retrying="findings.loading.value"
      />

      <DataTable
        v-else
        caption="Findings across the system"
        layer="page"
        :columns="findingColumns"
        :rows="findings.rows.value"
        :state="findingsTableState()"
        :pages="findings.pages.value"
        empty-message="No finding matches those filters."
        note="That is the answer a healthy system gives."
      >
        <template #severity="{ row }: { row: Finding }">
          <StatusBadge :status="FINDING_SEVERITY_PRESENTATION[row.severity]" />
        </template>
        <template #status="{ row }: { row: Finding }">
          <StatusBadge :status="FINDING_STATUS_PRESENTATION[row.status]" />
        </template>
        <template #subject="{ row }: { row: Finding }">
          <span class="font-mono text-xs">{{ subjectText(row) }}</span>
        </template>
        <template #detected_at="{ row }: { row: Finding }">
          <InstantCell :value="row.detected_at" />
        </template>
        <template #last_seen_at="{ row }: { row: Finding }">
          <InstantCell :value="row.last_seen_at" />
        </template>
      </DataTable>
    </section>

    <!--
      The evidence, per finding, as the server's own JSON. Formatted for a reader
      and never parsed into columns: the shape is this plane's check's, and a
      summary built from it would be a second reading of the figures with
      nothing to check it against.
    -->
    <section
      v-for="finding in findings.rows.value"
      v-show="finding.observed !== undefined"
      :key="`evidence-${finding.id}`"
      :aria-label="`Evidence for finding ${finding.id}`"
      class="flex flex-col gap-1"
    >
      <Card :title="`Evidence — ${finding.check_kind}`" :description="finding.detail">
        <pre class="overflow-x-auto rounded-md bg-muted p-3 font-mono text-xs text-foreground">{{
          JSON.stringify(finding.observed, null, 2)
        }}</pre>
      </Card>
    </section>
  </Stack>
</template>
