// Commerce: what this account has bought and what it has been granted. // // Three lists, and the
distinction between them is the whole reason this screen // exists rather than one table called
"billing". A **subscription** is a // recurring arrangement on a plan version. An **entitlement** is
one cycle of // one grant inside that arrangement — it is granted, it has a period, and it //
expires when the period ends, which is not the same moment the subscription // ends. A **bucket** is
money, and a bucket funded by an entitlement is not // the same bucket as a pay-as-you-go one.
Nothing on this page adds two of // those together. // // **The grant figure is a rendered value.**
`granted_minor_units` is what the // server granted; the page does not multiply it by a cycle count,
a plan price // or anything else it holds. A grant's amount is the whole of a plan's money // for
one cycle, and the reason that is worth saying out loud is that a // console which multiplies a
grant by the number of cycles would produce a // figure the contract never promised and no server
could contradict.
<script setup lang="ts">
import { Card, PageHeader, Stack } from "@ecoma-io/loom";
import { computed } from "vue";

import DataTable from "@/components/DataTable.vue";
import { fetchEntitlements, fetchSubscriptions } from "@/lib/api";
import { usePagedList } from "@/lib/paged-list";
import type { DataTableColumn } from "@/components/data-table";
import FailureView from "@/modules/failure/FailureView.vue";
import MoneyAmount from "@/modules/money/MoneyAmount.vue";
import InstantCell from "@/modules/status/InstantCell.vue";
import StatusBadge from "@/modules/status/StatusBadge.vue";
import {
  ENTITLEMENT_STATE_PRESENTATION,
  SUBSCRIPTION_STATE_PRESENTATION,
} from "@/modules/status/presentation";
import type {
  Entitlement,
  EntitlementPage,
  ListEntitlementsData,
  ListSubscriptionsData,
  Subscription,
  SubscriptionPage,
} from "@ecoma-io/llm-gateway-console-api-client";

const subscriptions = usePagedList<SubscriptionPage, ListSubscriptionsData["query"]>({
  read: (query) => fetchSubscriptions({ query }),
  shape: { filters: [] },
  vocabulary: {},
});

const entitlements = usePagedList<EntitlementPage, ListEntitlementsData["query"]>({
  read: (query) => fetchEntitlements({ query }),
  shape: { filters: [] },
  vocabulary: {},
});

const subscriptionColumns: readonly DataTableColumn[] = [
  { key: "id", label: "Subscription", class: "font-mono text-xs" },
  { key: "plan_version_id", label: "Plan version", class: "font-mono text-xs" },
  { key: "state", label: "State" },
  { key: "current_period_end", label: "Period ends" },
  { key: "cancel_at", label: "Ends" },
];

const entitlementColumns: readonly DataTableColumn[] = [
  { key: "id", label: "Entitlement", class: "font-mono text-xs" },
  { key: "subscription_id", label: "Subscription", class: "font-mono text-xs" },
  { key: "cycle", label: "Cycle", align: "right" },
  { key: "scope", label: "Scope" },
  { key: "granted", label: "Granted", align: "right" },
  { key: "state", label: "State" },
  { key: "period_end", label: "Period ends" },
];

/**
 * The grant figure, rendered from the row's own field.
 *
 * `granted_minor_units` is optional because a grant may be scoped to something
 * this plane does not meter — and an absent grant is NOT zero. A grant of zero
 * is a real, meaningful thing; the absence of the field is a fact about the
 * grant's dimension, and rendering it as `0` would assert the first when the
 * server said the second. The money component renders the unrenderable
 * sentinel for an absent amount, and the cell below says which case it is.
 */
function grantedAmount(row: Entitlement): { minor_units: number } | undefined {
  return row.granted_minor_units === undefined
    ? undefined
    : { minor_units: row.granted_minor_units };
}

/**
 * A scheduled cancellation is DATA, not a state (the contract says so on the
 * field), so it gets a column of its own: a reader looking for "when does this
 * end" must not have to read a `cancel_at` inside a `cancelled` badge.
 *
 * The explanation appears only when a `cancel_at` is actually in the future.
 * A year is not a substitute for that test: the console has no clock the
 * contract trusts, and the previous version compared against
 * `getUTCFullYear() - 1` and so described an `Ends` of 2025 — already past — as
 * "the subscription stays active and usable until that instant passes". It is
 * the ONLY moment that decides whether the sentence is true, and it is a field
 * on the row rather than a number the page invents.
 */
const pendingCancellation = computed(() =>
  subscriptions.rows.value.some(
    (row) =>
      row.cancel_at !== null &&
      row.cancel_at !== undefined &&
      row.cancel_at > new Date().toISOString(),
  ),
);
</script>

<template>
  <Stack gap="lg">
    <PageHeader
      title="Commerce"
      description="What this account has bought, and what each cycle granted."
    />

    <section aria-labelledby="commerce-subscriptions" class="flex flex-col gap-3">
      <h2 id="commerce-subscriptions" class="text-lg font-semibold text-foreground">
        Subscriptions
      </h2>

      <FailureView
        v-if="subscriptions.failure.value"
        :failure="subscriptions.failure.value"
        :on-retry="() => subscriptions.run()"
        :retrying="subscriptions.loading.value"
      />

      <DataTable
        v-else
        caption="Subscriptions in this account"
        layer="page"
        :columns="subscriptionColumns"
        :rows="subscriptions.rows.value"
        :state="subscriptions.loading.value ? 'loading' : 'empty'"
        :pages="subscriptions.pages.value"
        empty-message="This account has no subscription."
        note="A subscription is a recurring arrangement on a plan version. A pay-as-you-go balance is a different kind of thing, and it is on the accounting screen."
      >
        <template #state="{ row }: { row: Subscription }">
          <StatusBadge :status="SUBSCRIPTION_STATE_PRESENTATION[row.state]" />
        </template>
        <template #current_period_end="{ row }: { row: Subscription }">
          <InstantCell :value="row.current_period_end" />
        </template>
        <template #cancel_at="{ row }: { row: Subscription }">
          <InstantCell :value="row.cancel_at" />
        </template>
      </DataTable>

      <p v-if="pendingCancellation" class="text-xs text-muted-foreground">
        An "Ends" date in the future is a scheduled cancellation, not a state: the subscription
        stays active and usable until that instant passes.
      </p>
    </section>

    <section aria-labelledby="commerce-entitlements" class="flex flex-col gap-3">
      <h2 id="commerce-entitlements" class="text-lg font-semibold text-foreground">Entitlements</h2>

      <FailureView
        v-if="entitlements.failure.value"
        :failure="entitlements.failure.value"
        :on-retry="() => entitlements.run()"
        :retrying="entitlements.loading.value"
      />

      <DataTable
        v-else
        caption="Grants in this account, one row per cycle"
        layer="page"
        :columns="entitlementColumns"
        :rows="entitlements.rows.value"
        :state="entitlements.loading.value ? 'loading' : 'empty'"
        :pages="entitlements.pages.value"
        empty-message="No grant has been made to this account."
        note="A grant is one cycle of one plan, and it expires with its period."
      >
        <template #scope="{ row }: { row: Entitlement }">
          <span v-if="row.scope !== undefined" class="font-mono text-xs">{{ row.scope }}</span>
          <span v-else class="text-xs text-muted-foreground">Unscoped</span>
        </template>
        <template #granted="{ row }: { row: Entitlement }">
          <span class="inline-flex items-baseline gap-2">
            <MoneyAmount :amount="grantedAmount(row)" />
            <span class="text-xs text-muted-foreground">minor units</span>
          </span>
        </template>
        <template #state="{ row }: { row: Entitlement }">
          <StatusBadge :status="ENTITLEMENT_STATE_PRESENTATION[row.state]" />
        </template>
        <template #period_end="{ row }: { row: Entitlement }">
          <InstantCell :value="row.period_end" />
        </template>
      </DataTable>
    </section>

    <Card>
      <p class="text-sm text-muted-foreground">
        The figures on this screen are the server's, rendered one field at a time. This page adds
        nothing to them: no plan total, no per-cycle projection, and no balance of its own. What the
        money has done is on the accounting screen, as legs in a ledger.
      </p>
    </Card>
  </Stack>
</template>
