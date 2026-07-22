// Те же шрифты, что на сайте: Unbounded (заголовки) + Onest (текст).
// @remotion/google-fonts тянет их из каталога Google — офлайн-кэш Remotion.
import {loadFont as loadDisplay} from '@remotion/google-fonts/Unbounded';
import {loadFont as loadBody} from '@remotion/google-fonts/Onest';

export const display = loadDisplay().fontFamily;
export const body = loadBody().fontFamily;
