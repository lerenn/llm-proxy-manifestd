import { h } from 'vue'
import DefaultTheme from 'vitepress/theme'
import HelmPodOverview from './HelmPodOverview.vue'

export default {
  extends: DefaultTheme,
  Layout: () =>
    h(DefaultTheme.Layout, null, {
      'home-hero-after': () => h(HelmPodOverview),
    }),
}
