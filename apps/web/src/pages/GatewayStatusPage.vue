<script setup lang="ts">
// This is a purposeful foundation page, not a dashboard: health and readiness
// are the only two operations the current OpenAPI contract defines. Calling
// them through lib/api means every field still comes from generated types.
import { Badge, Button, Card, ErrorState, LoadingState, PageHeader, Stack } from "@ecoma-io/loom";
import { computed, onMounted, ref } from "vue";

import { getHealth, getReadiness, type HealthStatus } from "@/lib/api";
import { probeStatusVariant } from "@/lib/probe-status";

type ProbeName = "health" | "readiness";
type ProbeState = {
  error: boolean;
  loading: boolean;
  status?: HealthStatus["status"];
};

const probes = ref<Record<ProbeName, ProbeState>>({
  health: { error: false, loading: true },
  readiness: { error: false, loading: true },
});

const probeCards = computed(() => [
  {
    description: "Whether the API process is able to serve traffic.",
    key: "health" as const,
    title: "Liveness",
  },
  {
    description: "Whether the API service may receive traffic.",
    key: "readiness" as const,
    title: "Readiness",
  },
]);

async function refresh() {
  probes.value = {
    health: { error: false, loading: true },
    readiness: { error: false, loading: true },
  };

  const [health, readiness] = await Promise.all([getHealth(), getReadiness()]);

  probes.value = {
    health: {
      error: health.error !== undefined || health.data === undefined,
      loading: false,
      status: health.data?.status,
    },
    readiness: {
      error: readiness.error !== undefined || readiness.data === undefined,
      loading: false,
      status: readiness.data?.status,
    },
  };
}

onMounted(refresh);
</script>

<template>
  <Stack gap="lg" class="max-w-3xl">
    <PageHeader
      title="Gateway status"
      description="The live state of the API probes defined by the current gateway contract."
    >
      <template #actions>
        <Button :loading="probes.health.loading || probes.readiness.loading" @click="refresh">
          Refresh status
        </Button>
      </template>
    </PageHeader>

    <section aria-label="API probe status" class="grid gap-4 sm:grid-cols-2">
      <Card
        v-for="probe in probeCards"
        :key="probe.key"
        :description="probe.description"
        :title="probe.title"
      >
        <LoadingState v-if="probes[probe.key].loading" label="Checking probe" />
        <ErrorState
          v-else-if="probes[probe.key].error"
          title="Probe unavailable"
          description="The gateway did not return a successful probe response."
        />
        <Badge v-else :variant="probeStatusVariant(probes[probe.key].status)">{{
          probes[probe.key].status
        }}</Badge>
      </Card>
    </section>
  </Stack>
</template>
