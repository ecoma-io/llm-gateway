<script setup lang="ts">
import { useCounterStore } from "@/stores/counter";

// The counter is what proves Pinia is live in the running app, not only in a
// test — the first real store replaces it.
const counter = useCounterStore();
</script>

<template>
  <main>
    <p>Gateway scaffold — the console is not built yet.</p>
    <p>
      <span data-testid="count">{{ counter.count }}</span>
      <button type="button" @click="counter.increment()">Increment</button>
    </p>
  </main>
</template>
