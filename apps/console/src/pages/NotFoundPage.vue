<script setup lang="ts">
// A URL the console does not own. It says so and offers the two places that
// always exist, rather than rendering a dead end: a 404 that explains nothing
// is the one screen where an operator has no next move offered.
import { Card, Stack } from "@ecoma-io/loom";
import { RouterLink } from "vue-router";
</script>

<template>
  <Stack gap="md" class="max-w-lg">
    <h1 class="text-2xl font-semibold text-foreground">That page is not part of the console</h1>
    <p class="text-muted-foreground">
      The address you followed does not name a screen. The pages that do are in the navigation on
      the left.
    </p>
    <Card title="Where to go next">
      <nav aria-label="Suggested destinations" class="flex flex-wrap gap-3">
        <RouterLink
          to="/"
          class="inline-flex min-h-11 items-center rounded-md border border-border px-4 text-sm underline-offset-4 hover:underline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring"
        >
          Dashboard
        </RouterLink>
        <RouterLink
          to="/accounting"
          class="inline-flex min-h-11 items-center rounded-md border border-border px-4 text-sm underline-offset-4 hover:underline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring"
        >
          Accounting
        </RouterLink>
      </nav>
    </Card>
  </Stack>
</template>
