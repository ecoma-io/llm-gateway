// The router is functional before a product domain arrives, so the first
// designed page is a route addition rather than a wiring change. Static import
// is deliberate: one page has no split point to justify lazy-loading plumbing.
import { createRouter, createWebHistory } from "vue-router";

import GatewayStatusPage from "@/pages/GatewayStatusPage.vue";

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    {
      path: "/",
      name: "gateway-status",
      component: GatewayStatusPage,
    },
  ],
});

export default router;
