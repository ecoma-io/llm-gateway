// Accounting: the buckets, and one bucket's ledger. // // **The rule this screen exists to hold is
that it computes nothing.** A bucket // carries three figures — `settled`, `held`, `available` — and
the three // identities the domain keeps between them are the SERVER's to keep. This page // renders
each from its own field and reads no other one to produce it. The // temptation is exactly the
arithmetic a reader would do in their head, and // doing it here would make a client-side `settled -
held` a second ledger with // no audit behind it; when it disagreed with the server there would be
no way to // say which was right. `formatBalances` is a view over the response for the same //
reason, and the assertion that matters is the one a sum would fail. // // **A bucket is shown per
bucket, and the ledger is per bucket too.** A PAYG // bucket and an entitlement bucket are never one
balance — a cycle that rolls // must not take the pay-as-you-go balance with it — so this screen
opens no // cross-bucket view and offers no total. The reader picks a bucket from the list // and
reads that bucket's history, which is what a ledger is a record of. // // **The ledger's legs are
signed, and the sign is read.** A `consume` leg's // `settled_delta` is negative in the server's own
storage, and this page renders // the sign it was given. It does not negate anything for
readability, because a // client that flipped a sign is a client whose ledger disagrees with the
domain // over what a `consume` means.
<script setup lang="ts">
import { Card, PageHeader, SegmentedControl, Stack } from "@ecoma-io/loom";
import { computed } from "vue";

import DataTable from "@/components/DataTable.vue";
import { fetchFundingBuckets, fetchLedgerEntries } from "@/lib/api";
import { usePagedList } from "@/lib/paged-list";
import { useQueryState } from "@/lib/query-state";
import type { DataTableColumn } from "@/components/data-table";
import FailureView from "@/modules/failure/FailureView.vue";
import MoneyAmount from "@/modules/money/MoneyAmount.vue";
import InstantCell from "@/modules/status/InstantCell.vue";
import StatusBadge from "@/modules/status/StatusBadge.vue";
import {
  FUNDING_BUCKET_STATUS_PRESENTATION,
  LEDGER_KIND_PRESENTATION,
  ledgerDirectionPresentation,
} from "@/modules/status/presentation";
import type {
  FundingBucket,
  FundingBucketPage,
  LedgerEntry,
  LedgerEntryPage,
  ListFundingBucketsData,
  ListLedgerEntriesData,
} from "@ecoma-io/llm-gateway-console-api-client";

const buckets = usePagedList<FundingBucketPage, ListFundingBucketsData["query"]>({
  read: (query) => fetchFundingBuckets({ query }),
  shape: { filters: [], cursor: "after" },
  vocabulary: {},
});

/**
 * The selected bucket, in the route.
 *
 * A bucket id in a URL is a PATH SEGMENT on the wire — the ledger operation
 * takes one and the contract's cross-account answer for a bucket this account
 * does not own is `404`, not a `403` — but it is a segment and not a query
 * parameter here, because on this screen it selects which of the page's own
 * tables to show rather than filtering one. It is the only resource id that
 * appears in a console URL and it is a bucket, so a link to this screen can
 * never disclose an account, a user or a credential.
 *
 * The filter list is what keeps it out of the pager's links: `pagerFor` carries
 * only declared filters, so a page change on the bucket list is a bare `?after`
 * and never a bucket id this screen did not choose.
 */
const LEDGER_KINDS: readonly LedgerEntry["kind"][] = [
  "grant",
  "topup",
  "hold",
  "release",
  "consume",
  "adjustment",
];

const { filter, setFilter } = useQueryState(
  { filters: ["bucket", "kind"] },
  // `kind` is validated against the ledger's own closed vocabulary, so a
  // hand-edited `?kind=not-a-kind` leaves the segmented control with nothing
  // marked — the request is unfiltered, and the control now says so too.
  // `bucket` has no vocabulary: the screen chooses it, from a list it read.
  { kind: LEDGER_KINDS },
);
const selectedBucketId = computed(() => filter("bucket") ?? "");

/**
 * The ledger read, which is the one read on this surface with a path parameter
 * and therefore the one place a list's operation is not simply a query.
 *
 * `read` closes over the screen's selected bucket rather than taking it from
 * the query object, because on the wire it is `path.funding_bucket_id` and not
 * a query parameter at all. That is also why this composable is called only
 * once a bucket is chosen — a read with an empty path would ask the server
 * about a bucket that does not exist, and a screen that had to render a `404`
 * for "you have not clicked anything yet" would be reporting its own state as a
 * server fact. The gate is `ledgerOpen` below, and the `v-if` it drives is the
 * same predicate, so the read cannot happen without the table it feeds.
 */
const ledger = usePagedList<LedgerEntryPage, ListLedgerEntriesData["query"]>({
  read: (query) =>
    fetchLedgerEntries({ path: { funding_bucket_id: selectedBucketId.value }, query }),
  // `kind` is a filter of the LEDGER, and it rides the same query the pager
  // carries — so a kind change drops the cursor, which is the whole reason a
  // cursor carries a filter fingerprint. `bucket` is NOT declared: the screen
  // sets it when the reader picks a bucket, and a filter control that could set
  // it back would let the two tables disagree — which is also why it is
  // CARRIED through the ledger's own navigations rather than dropped.
  //
  // The ledger's cursor is `ledger_after`, not `after`. Both tables are on this
  // one route, and a single route-global cursor key means the bucket list's
  // Next writes a position the ledger then sends against its own operation —
  // a cursor the server issued for a different collection, answered with `400
  // invalid_request`. A shared key would have been the bug; the second key is
  // the fix, and the two tables now position independently.
  shape: { filters: ["kind"], cursor: "ledger_after" },
  vocabulary: { kind: LEDGER_KINDS },
  // The gate, stated here where the comment above explains it. Without it the
  // composable read on mount with an EMPTY path id, and the server answered for
  // a bucket the reader had not named — while the section rendered "choose a
  // bucket", so the screen was reporting its own state as a server fact.
  enabled: () => selectedBucketId.value !== "",
});

const kindOptions = [
  { value: "", label: "All" },
  ...LEDGER_KINDS.map((kind) => ({ value: kind, label: LEDGER_KIND_PRESENTATION[kind].label })),
];

const kindValue = computed({
  get: () => filter("kind") ?? "",
  set: (value: string) => setFilter("kind", value),
});

/**
 * The bucket whose ledger is open, and the one table that reads it.
 *
 * A ledger with no bucket named is not an error and not an empty list — it is
 * no ledger at all, so the table is not rendered and the screen says which
 * bucket to pick. That is a different sentence from "this bucket has no legs",
 * and conflating them would tell an operator their money moved nowhere when in
 * fact they had not opened a bucket.
 */
const selectedBucket = computed<FundingBucket | undefined>(() =>
  buckets.rows.value.find((row) => row.id === selectedBucketId.value),
);

const ledgerOpen = computed(() => selectedBucketId.value !== "");

const ledgerColumns: readonly DataTableColumn[] = [
  { key: "sequence", label: "Seq", align: "right" },
  { key: "kind", label: "Leg" },
  { key: "direction", label: "Direction" },
  { key: "settled_delta", label: "Settled Δ", align: "right" },
  { key: "held_delta", label: "Held Δ", align: "right" },
  { key: "created_at", label: "Recorded" },
];

const bucketColumns: readonly DataTableColumn[] = [
  { key: "id", label: "Bucket", class: "font-mono text-xs" },
  { key: "kind", label: "Kind" },
  { key: "balances", label: "Balances", class: "font-mono text-xs" },
  { key: "status", label: "Status" },
  { key: "version", label: "Version", align: "right" },
];
</script>

<template>
  <Stack gap="lg">
    <PageHeader
      title="Accounting"
      description="The balances this account holds, and the legs that moved them."
    />

    <section aria-labelledby="accounting-buckets" class="flex flex-col gap-3">
      <h2 id="accounting-buckets" class="text-lg font-semibold text-foreground">Funding buckets</h2>

      <FailureView
        v-if="buckets.failure.value"
        :failure="buckets.failure.value"
        :on-retry="() => buckets.run()"
        :retrying="buckets.loading.value"
      />

      <DataTable
        v-else
        caption="Funding buckets, with settled, held and available in minor units"
        layer="page"
        :columns="bucketColumns"
        :rows="buckets.rows.value"
        :state="buckets.loading.value ? 'loading' : 'empty'"
        :pages="buckets.pages.value"
        empty-message="This account holds no funding bucket."
        note="A bucket is money set aside for spend. It comes from a plan's cycle or from a top-up — never from the other."
      >
        <template #balances="{ row }: { row: FundingBucket }">
          <span class="inline-flex items-center gap-2">
            <MoneyAmount :amount="row.balances.settled" />
            <span aria-hidden="true" class="text-muted-foreground">/</span>
            <MoneyAmount :amount="row.balances.held" />
            <span aria-hidden="true" class="text-muted-foreground">/</span>
            <MoneyAmount :amount="row.balances.available" />
          </span>
        </template>
        <template #status="{ row }: { row: FundingBucket }">
          <StatusBadge :status="FUNDING_BUCKET_STATUS_PRESENTATION[row.status]" />
        </template>
        <template #id="{ row }: { row: FundingBucket }">
          <button
            type="button"
            class="min-h-11 rounded-md px-2 font-mono text-xs underline-offset-4 hover:underline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring"
            :aria-pressed="row.id === selectedBucketId"
            @click="setFilter('bucket', row.id === selectedBucketId ? '' : row.id)"
          >
            {{ row.id }}
          </button>
        </template>
      </DataTable>

      <p class="text-xs text-muted-foreground">
        Each column reads its own figure from the server: settled, held, available. This page does
        not subtract one to obtain another, and there is no total across buckets — each is a
        separate balance with a separate owner.
      </p>
    </section>

    <section aria-labelledby="accounting-ledger" class="flex flex-col gap-3">
      <h2 id="accounting-ledger" class="text-lg font-semibold text-foreground">Ledger</h2>

      <p v-if="!ledgerOpen" class="text-sm text-muted-foreground">
        Choose a bucket above to read the legs that moved it. A ledger is one bucket's history and
        never a merge of several.
      </p>

      <template v-else>
        <Card>
          <p class="text-xs text-muted-foreground">
            Bucket
            <span class="font-mono">{{ selectedBucketId }}</span>
            <span v-if="selectedBucket">
              — kind <span class="font-mono">{{ selectedBucket.kind }}</span
              >, opened
              <InstantCell :value="selectedBucket.opened_at" />
            </span>
          </p>
        </Card>

        <SegmentedControl
          v-model="kindValue"
          :options="kindOptions"
          aria-label="Filter legs by kind"
        />

        <FailureView
          v-if="ledger.failure.value"
          :failure="ledger.failure.value"
          :on-retry="() => ledger.run()"
          :retrying="ledger.loading.value"
        />

        <DataTable
          v-else
          :caption="`Ledger legs for bucket ${selectedBucketId}`"
          layer="page"
          :columns="ledgerColumns"
          :rows="ledger.rows.value"
          :state="ledger.loading.value ? 'loading' : 'empty'"
          :pages="ledger.pages.value"
          empty-message="This bucket has no leg matching that kind."
        >
          <template #kind="{ row }: { row: LedgerEntry }">
            <StatusBadge :status="LEDGER_KIND_PRESENTATION[row.kind]" />
          </template>
          <template #direction="{ row }: { row: LedgerEntry }">
            <StatusBadge
              :status="
                ledgerDirectionPresentation(
                  row.settled_delta.minor_units,
                  row.held_delta.minor_units,
                )
              "
            />
          </template>
          <template #settled_delta="{ row }: { row: LedgerEntry }">
            <MoneyAmount :amount="row.settled_delta" signed />
          </template>
          <template #held_delta="{ row }: { row: LedgerEntry }">
            <MoneyAmount :amount="row.held_delta" signed />
          </template>
          <template #created_at="{ row }: { row: LedgerEntry }">
            <InstantCell :value="row.created_at" />
          </template>
        </DataTable>
      </template>
    </section>
  </Stack>
</template>
