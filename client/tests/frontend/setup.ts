import { cleanup } from '@testing-library/react';
import { afterEach, beforeEach } from 'vitest';
import { setInterfaceLanguage } from '../../src/i18n';
beforeEach(()=>{setInterfaceLanguage('fr');document.documentElement.dataset.theme='dark';document.documentElement.dataset.reduceMotion='false';});

afterEach(() => cleanup());
