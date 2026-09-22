// The canonical minimal store: it exists so Pinia is initialized AND
// exercised, and is replaced by the first real store.
import { ref } from "vue";
import { defineStore } from "pinia";

export const useCounterStore = defineStore("counter", () => {
  const count = ref(0);

  function increment() {
    count.value += 1;
  }

  return { count, increment };
});
