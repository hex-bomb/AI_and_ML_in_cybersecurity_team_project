"use client"

// Скопирован 1-в-1 из desktop/src/components/ui/input.tsx.
// Единственное изменение — rounded-xl уже был в оригинале.

import * as React from "react";
import { cn } from "../../lib/utils";

export interface InputProps extends React.InputHTMLAttributes<HTMLInputElement> {
  label?: string;
}

const Input = React.forwardRef<HTMLInputElement, InputProps>(
  ({ className, type, label, id, placeholder, ...props }, ref) => {
    const floatingLabel = label ?? placeholder;
    const inputId = id || floatingLabel?.toLowerCase().replace(/\s+/g, "-");

    return (
      <fieldset className={cn("relative w-full border border-border/50 rounded-xl bg-background/95", className)}>
        <input
          type={type}
          id={inputId}
          className={cn(
            "flex h-[3rem] w-full rounded-xl bg-transparent px-3 text-sm text-foreground placeholder:text-transparent ring-offset-background file:border-0 file:bg-transparent file:text-sm file:font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50 peer pt-[13px] pb-[3px] placeholder-shown:pt-2 placeholder-shown:pb-2 focus:pt-[13px] focus:pb-[3px]",
            className
          )}
          ref={ref}
          placeholder=" "
          {...props}
        />
        {floatingLabel && (
          <legend className={cn(
            "absolute left-2 top-1/2 -translate-y-1/2 text-sm text-muted-foreground px-1 pointer-events-none transition-all duration-200 ease-out whitespace-nowrap",
            "peer-focus:top-[11px] peer-focus:-translate-y-1/2 peer-focus:text-xs peer-focus:text-primary",
            "peer-[:not(:placeholder-shown)]:top-[11px] peer-[:not(:placeholder-shown)]:-translate-y-1/2 peer-[:not(:placeholder-shown)]:text-xs peer-[:not(:placeholder-shown)]:text-muted-foreground",
            "peer-placeholder-shown:top-1/2 peer-placeholder-shown:-translate-y-1/2 peer-placeholder-shown:text-sm",
            "peer-disabled:opacity-50 peer-disabled:cursor-not-allowed"
          )}>
            {floatingLabel}
          </legend>
        )}
      </fieldset>
    );
  }
);
Input.displayName = "Input";

export { Input };