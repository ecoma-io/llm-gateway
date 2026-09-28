<script setup lang="ts">
// Catalog: the plans this gateway sells.
//
// The name is the catalog screen. ADR 0012 §5 defers the model catalog — a
// model's catalogue, its prices and its availability are Data Plane state with
// no operation on `console.yaml` — so what remains is the plan list, and this
// screen says so rather than reading as the model catalog with its contents
// removed. A nav label of "Catalog" beside a table of plans invites exactly
// the question this plane cannot answer, so the page header answers it in one
// line.
//
// The list is whole-plane: a plan is not an account's, and the operation takes
// no account and no filter. Nothing here is filtered, which is why the pager
// carries no query parameters at all and why there is no filter control.
import { PageHeader, Stack } from "@ecoma-io/loom";

import DataTable from "@/components/DataTable.vue";
import { fetchPlans } from "@/lib/api";
import { usePagedList } from "@/lib/paged-list";
import type { DataTableColumn } from "@/components/data-table";
import FailureView from "@/modules/failure/FailureView.vue";
import InstantCell from "@/modules/status/InstantCell.vue";
import type { ListPlansData, Plan, PlanPage } from "@ecoma-io/llm-gateway-console-api-client";

const plans = usePagedList<PlanPage, ListPlansData["query"]>({
  read: (query) => fetchPlans({ query }),
  shape: { filters: [] },
  vocabulary: {},
});

const columns: readonly DataTableColumn[] = [
  { key: "name", label: "Plan" },
  { key: "id", label: "Id", class: "font-mono text-xs" },
  { key: "created_at", label: "Created" },
];

/**
 * The table's state word, and it is three states rather than the two
 * `loading ? "loading" : "empty"` would give.
 *
 * The two-state reading fails in the direction a reader is harmed: `usePagedList`
 * runs its read from `onMounted`, which is AFTER the first render, so on the
 * first paint nothing has been asked and nothing has been answered —
 * `loading` is false, `data` is `undefined`, `rows` is empty. "Empty" is
 * therefore not the absence of plans, it is the absence of an answer, and the
 * table would print "This gateway has published no plan." over a fact it has not
 * established. A gateway that is simply slow to answer gets accused of selling
 * nothing.
 *
 * `data === undefined` is the one probe that tells those apart, and it is the
 * probe `resource.ts` is built around: the file's own header names "nothing to
 * show" as the third state, and this is the third state.
 */
function tableState(): "ready" | "loading" | "empty" {
  if (plans.loading.value) return "loading";
  if (plans.data.value === undefined) return "loading";
  return plans.rows.value.length === 0 ? "empty" : "ready";
}
</script>

<template>
  <Stack gap="lg">
    <PageHeader
      title="Catalog"
      description="The plans this gateway sells. The model catalog — what a model is called, what it costs, whether it is available — is Data Plane state and has no operation here yet."
    />

    <FailureView
      v-if="plans.failure.value"
      :failure="plans.failure.value"
      :on-retry="() => plans.run()"
      :retrying="plans.loading.value"
    />

    <DataTable
      v-else
      caption="Plans"
      layer="page"
      :columns="columns"
      :rows="plans.rows.value"
      :state="tableState()"
      :pages="plans.pages.value"
      empty-message="This gateway has published no plan."
    >
      <template #created_at="{ row }: { row: Plan }">
        <InstantCell :value="row.created_at" />
      </template>
    </DataTable>
  </Stack>
</template>
