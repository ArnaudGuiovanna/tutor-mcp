import { cleanup } from '@testing-library/react';
import { afterEach, beforeEach } from 'vitest';
import { setInterfaceLanguage } from '../../src/i18n';
beforeEach(()=>{Object.defineProperty(window,'innerWidth',{value:1280,writable:true,configurable:true});localStorage.clear();setInterfaceLanguage('fr');document.documentElement.dataset.theme='dark';document.documentElement.dataset.reduceMotion='false';});

afterEach(() => cleanup());
