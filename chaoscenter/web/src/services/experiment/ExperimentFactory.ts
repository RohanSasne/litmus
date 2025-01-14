import { InfrastructureType } from '@api/entities';
import type { ExperimentYamlService } from './ExperimentYamlService';

class ExperimentFactory {
  private map: Map<InfrastructureType, ExperimentYamlService>;

  constructor() {
    this.map = new Map();
  }

  public registerInfrastructureTypeHandler(
    infrastructureType: InfrastructureType,
    handler: ExperimentYamlService
  ): void {
    this.map.set(infrastructureType, handler);
  }

  public getInfrastructureTypeHandler(): ExperimentYamlService | undefined {
    return this.map.get(InfrastructureType.KUBERNETES);
  }
}

export default new ExperimentFactory();
